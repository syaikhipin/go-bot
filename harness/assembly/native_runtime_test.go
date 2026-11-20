package assembly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/interaction"
	"go-bot/harness/permission"
	"go-bot/harness/plan"
	"go-bot/harness/schedule"
	"go-bot/harness/session"
	"go-bot/internal/testsupport/session"
)

type nativeMemoryStorage struct {
	mu     sync.Mutex
	header sessionformat.Header
	events []sessionformat.Event
}

type nativeTreeStorage struct {
	*nativeMemoryStorage
	mu       sync.Mutex
	children map[string]*nativeMemoryStorage
}

func nativeMemoryJSONL(t *testing.T, storage *nativeMemoryStorage) []byte {
	t.Helper()
	storage.mu.Lock()
	header := storage.header
	events := append([]sessionformat.Event(nil), storage.events...)
	storage.mu.Unlock()
	first, err := sessionformat.EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	output.Write(first)
	output.WriteByte('\n')
	for index, event := range events {
		line, err := sessionformat.EncodeEvent(event, uint64(index))
		if err != nil {
			t.Fatal(err)
		}
		output.Write(line)
		output.WriteByte('\n')
	}
	return output.Bytes()
}

func (s *nativeTreeStorage) ChildNativeStorage(childID string) (NativeStorage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.children == nil {
		s.children = make(map[string]*nativeMemoryStorage)
	}
	child := s.children[childID]
	if child == nil {
		child = &nativeMemoryStorage{}
		s.children[childID] = child
	}
	return child, nil
}

type nativeMemoryJournal struct{ store *nativeMemoryStorage }

func (j nativeMemoryJournal) Append(_ context.Context, events []sessionformat.Event) error {
	j.store.mu.Lock()
	defer j.store.mu.Unlock()
	if len(j.store.events) != int(events[0].Seq) {
		return errors.New("native journal position mismatch")
	}
	j.store.events = append(j.store.events, events...)
	return nil
}
func (nativeMemoryJournal) Close() error { return nil }

func (s *nativeMemoryStorage) CreateNative(_ context.Context, header sessionformat.Header) (sessionformat.EventJournal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.header.ID != "" {
		return nil, errors.New("native Session already exists")
	}
	s.header = header
	return nativeMemoryJournal{store: s}, nil
}

func (s *nativeMemoryStorage) OpenNative(context.Context) (sessionformat.EventJournal, sessionformat.PhysicalArtifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.header.ID == "" {
		return nil, sessionformat.PhysicalArtifact{}, errors.New("native Session does not exist")
	}
	events := append([]sessionformat.Event(nil), s.events...)
	return nativeMemoryJournal{store: s}, sessionformat.PhysicalArtifact{Header: s.header, Events: events}, nil
}

type nativeTestBuilder struct{ containerTestBuilder }

func (b nativeTestBuilder) BuildNative(ctx context.Context, identity NativeRuntimeIdentity, reason string) (Resources, error) {
	return b.Build(ctx, session.Header{Mode: "container", UserID: identity.UserID, SessionID: identity.SessionID}, reason)
}

type observingModel struct {
	mu      sync.Mutex
	prompts []string
	specs   [][]agent.ToolSpec
	routes  []agent.ModelRoute
}

func (m *observingModel) Complete(ctx context.Context, messages []agent.Message, specs []agent.ToolSpec) (session.AssistantMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	route, _ := agent.ModelRouteFromContext(ctx)
	m.routes = append(m.routes, route)
	var prompt string
	for _, message := range messages {
		if message.Role == "system" {
			prompt += message.Content
		}
	}
	m.prompts = append(m.prompts, prompt)
	m.specs = append(m.specs, append([]agent.ToolSpec(nil), specs...))
	return session.AssistantMessage{Content: "done"}, nil
}

type nativeGoalModel struct{ calls int }

func TestNativeFactoryPinsFullAccessIntoFreshSession(t *testing.T) {
	ctx := context.Background()
	storage := &nativeMemoryStorage{}
	factory := NativeFactory{
		Storage: storage, Builder: nativeTestBuilder{newContainerTestBuilder(t, t.TempDir())}, Model: &observingModel{},
		Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help with the user's task.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2},
	}
	identity := NativeRuntimeIdentity{UserID: "alice", SessionID: "permissions", Workspace: "/workspace"}
	live, err := factory.Activate(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	wantTypes := []string{"permission/preset", "sandbox/mode", "approval/policy"}
	wantData := []string{`{"preset":"danger-full-access"}`, `{"mode":"danger-full-access"}`, `{"policy":"never"}`}
	events := live.Session.DurableEvents()
	if len(events) != len(wantTypes) {
		t.Fatalf("fresh Session events = %v", events)
	}
	for i, event := range events {
		if event.Type != wantTypes[i] || string(event.Data) != wantData[i] {
			t.Fatalf("permission event %d = %s %s", i, event.Type, event.Data)
		}
	}
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(nativeMemoryJSONL(t, storage))); err != nil {
		t.Fatalf("permission facts fail v4 inspection: %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := factory.Resume(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if got := len(resumed.Session.DurableEvents()); got != len(wantTypes)+1 { // resume adds session/end-seed
		t.Fatalf("resumed Session events = %d", got)
	}
}

func TestNativeForkRecordsDelegatedPermissionsAfterSeed(t *testing.T) {
	ctx := context.Background()
	storage := &nativeTreeStorage{nativeMemoryStorage: &nativeMemoryStorage{}}
	factory := NativeFactory{
		Storage: storage, Builder: nativeTestBuilder{newContainerTestBuilder(t, t.TempDir())}, Model: &observingModel{},
		Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help with the user's task.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2},
	}
	identity := NativeRuntimeIdentity{UserID: "alice", SessionID: "parent", Workspace: "/workspace"}
	parent, err := factory.Activate(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if _, err := parent.Agent.RunTextTurn(ctx, "Complete one turn"); err != nil {
		t.Fatal(err)
	}
	child, err := factory.ForkChild(ctx, identity, parent, "child", "Fork")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	counts := map[string]int{}
	for _, event := range child.Session.DurableEvents() {
		counts[event.Type]++
	}
	for _, kind := range []string{"permission/preset", "sandbox/mode", "approval/policy"} {
		if counts[kind] != 2 {
			t.Fatalf("forked child %s count = %d", kind, counts[kind])
		}
	}
	if counts["session/end-seed"] != 1 {
		t.Fatalf("forked child seed marker count = %d", counts["session/end-seed"])
	}
	var delegated []sessionformat.Event
	seedSeen := false
	for _, event := range child.Session.DurableEvents() {
		if event.Type == "session/end-seed" {
			seedSeen = true
			continue
		}
		if seedSeen && (event.Type == "sandbox/mode" || event.Type == "approval/policy" || event.Type == "permission/preset") {
			delegated = append(delegated, event)
		}
	}
	if len(delegated) != 3 || delegated[0].Type != "sandbox/mode" ||
		string(delegated[0].Data) != `{"mode":"danger-full-access","source":"delegation"}` ||
		delegated[1].Type != "approval/policy" ||
		string(delegated[1].Data) != `{"policy":"never","source":"delegation"}` ||
		delegated[2].Type != "permission/preset" {
		t.Fatalf("forked child delegation events = %+v", delegated)
	}
}

func (m *nativeGoalModel) Complete(_ context.Context, _ []agent.Message, _ []agent.ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	if m.calls == 1 {
		return session.AssistantMessage{ToolCalls: []session.ToolRequest{{
			ID: "create-goal-1", Name: "create_goal",
			Arguments: `{"objective":"Finish the report","max_goal_rounds":3}`,
		}}}, nil
	}
	return session.AssistantMessage{Content: "Goal created."}, nil
}

func TestToolPolicyCanRequireApprovalBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	factory := NativeFactory{
		Storage: &nativeMemoryStorage{}, Builder: nativeTestBuilder{newContainerTestBuilder(t, t.TempDir())},
		Model: &nativeGoalModel{}, Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help the user.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2},
		ToolPreExecutePolicy: func(_ context.Context, execution agent.ToolExecution) (agent.PreToolDecision, error) {
			if execution.Name != "create_goal" {
				return agent.PreToolDecision{Kind: "allow"}, nil
			}
			return agent.PreToolDecision{Kind: "ask", Reason: "Verify pre-dispatch approval"}, nil
		},
	}
	live, err := factory.Activate(ctx, NativeRuntimeIdentity{UserID: "alice", SessionID: "approval-test", Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := live.Agent.RunTextTurn(ctx, "Work on this report"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := live.Goals.Get(); err != nil || found {
		t.Fatalf("policy did not prevent dispatch: found=%t err=%v", found, err)
	}
	var asked, rejected bool
	for _, event := range live.Session.Events() {
		if event.Type == "approval/asked" {
			asked = true
		}
		if event.Type == "approval/decided" && strings.Contains(string(event.Data), "rejected") {
			rejected = true
		}
	}
	if !asked || !rejected {
		t.Fatalf("approval audit missing: asked=%t rejected=%t", asked, rejected)
	}
}

func TestInteractiveApprovalHasNoDeadlineAndSurvivesClientDetach(t *testing.T) {
	factory := NativeFactory{
		Storage: &nativeMemoryStorage{}, Builder: nativeTestBuilder{newContainerTestBuilder(t, t.TempDir())},
		Model: &nativeGoalModel{}, Provider: "test", ModelID: "test", SystemPrompt: "Help.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2},
		ToolPreExecutePolicy: func(context.Context, agent.ToolExecution) (agent.PreToolDecision, error) {
			return agent.PreToolDecision{Kind: "ask", Reason: "Test approval"}, nil
		},
	}
	live, err := factory.Activate(t.Context(), NativeRuntimeIdentity{UserID: "alice", SessionID: "no-approval-timeout", Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	live.Questions.Enable()
	if err := permission.Save(t.Context(), live.Session, permission.ReadOnly, false, false); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		detach := live.Questions.AttachApprovalClient()
		result := make(chan error, 1)
		go func() { _, err := live.Agent.RunTextTurn(ctx, "Create a report goal"); result <- err }()
		synctest.Wait()
		if len(live.Questions.Pending()) != 1 {
			t.Fatal("approval was not pending")
		}
		detach()
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("approval ended without a decision: %v", err)
		default:
		}
		pending := live.Questions.Pending()
		if len(pending) != 1 {
			t.Fatal("approval expired or disappeared after disconnect")
		}
		detach = live.Questions.AttachApprovalClient()
		defer detach()
		if err := live.Questions.Answer(ctx, pending[0].CallID, interaction.Answer{Answers: []interaction.AnswerItem{{ID: "approval", Selected: []string{permission.Approve}}}}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if _, found, err := live.Goals.Get(); err != nil || !found {
			t.Fatalf("late approval failed to execute: found=%t err=%v", found, err)
		}
	})
}

func TestNativeFactoryGoalToolWritesV4AndResumesDisarmed(t *testing.T) {
	ctx := context.Background()
	storage := &nativeMemoryStorage{}
	factory := NativeFactory{
		Storage: storage, Builder: nativeTestBuilder{newContainerTestBuilder(t, t.TempDir())}, Model: &nativeGoalModel{},
		Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help with the user's task.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2},
	}
	identity := NativeRuntimeIdentity{UserID: "alice", SessionID: "native-goal", Workspace: "/workspace"}
	live, err := factory.Activate(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Agent.RunTextTurn(ctx, "Work on this long-running report"); err != nil {
		t.Fatal(err)
	}
	view, found, err := live.Goals.Get()
	if err != nil || !found || view.Goal.Objective != "Finish the report" || view.Activation != "armed" {
		t.Fatalf("goal after native tool = %+v found=%t err=%v", view, found, err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	var changes int
	for _, event := range storage.events {
		if event.Type == "goal/change" {
			changes++
			if _, err := sessionformat.DecodeGoalChange(event.Data); err != nil {
				t.Fatalf("native goal tool wrote a non-v4 payload: %v", err)
			}
		}
	}
	if changes != 1 {
		t.Fatalf("native goal changes = %d", changes)
	}
	resumed, err := factory.Resume(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	cold, found, err := resumed.Goals.Get()
	if err != nil || !found || cold.Goal.ID != view.Goal.ID || cold.Activation != "disarmed" {
		t.Fatalf("cold native goal = %+v found=%t err=%v", cold, found, err)
	}
}

func TestNativeGoalToolDoesNotTreatJobInputAsHumanAuthority(t *testing.T) {
	ctx := context.Background()
	factory := NativeFactory{
		Storage: &nativeMemoryStorage{}, Builder: nativeTestBuilder{newContainerTestBuilder(t, t.TempDir())}, Model: &nativeGoalModel{},
		Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help with the user's task.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2},
	}
	live, err := factory.Activate(ctx, NativeRuntimeIdentity{
		UserID: "alice", SessionID: "job-goal", Workspace: "/workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := live.Agent.AcceptMessage(ctx, []byte(`{"id":"job-message","role":"user","source":{"kind":"job"},"content":[{"type":"text","text":"Job completed"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := live.Agent.RunNext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, err := live.Goals.Get(); err != nil || found {
		t.Fatalf("job input created a goal: found=%t err=%v", found, err)
	}
}

func TestNativeGoalQueuedRoundYieldsToHumanBeforeClaim(t *testing.T) {
	ctx := context.Background()
	factory := NativeFactory{
		Storage: &nativeMemoryStorage{}, Builder: nativeTestBuilder{newContainerTestBuilder(t, t.TempDir())}, Model: &observingModel{},
		Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help with the user's task.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2},
	}
	live, err := factory.Activate(ctx, NativeRuntimeIdentity{
		UserID: "alice", SessionID: "human-before-goal", Workspace: "/workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	created, err := live.Goals.Create(ctx, "Finish the report", 3)
	if err != nil {
		t.Fatal(err)
	}
	goalMessage, err := json.Marshal(map[string]any{
		"id": "queued-goal", "role": "user",
		"source":  map[string]any{"kind": "goal", "goalId": created.Goal.ID, "revision": created.Goal.Revision, "round": 1},
		"content": []map[string]string{{"type": "text", "text": "Continue goal"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Agent.AcceptMessage(ctx, goalMessage); err != nil {
		t.Fatal(err)
	}
	if _, err := live.Agent.AcceptText(ctx, "Human instruction takes precedence"); err != nil {
		t.Fatal(err)
	}
	if ran, _, err := live.Agent.RunNext(ctx); err != nil || !ran {
		t.Fatalf("human turn = ran=%t err=%v", ran, err)
	}
	projection, err := sessionformat.FoldGoal(live.Session.DurableEvents())
	if err != nil || projection.RoundsStarted != 0 {
		t.Fatalf("goal round was counted before claim: %+v, %v", projection, err)
	}
	var human, goalAdmitted, canceled int
	for _, event := range live.Session.DurableEvents() {
		if event.Type == "user/message" {
			var data struct {
				Source struct {
					Kind string `json:"kind"`
				} `json:"source"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Source.Kind == "user" {
				human++
			} else if data.Source.Kind == "goal" {
				goalAdmitted++
			}
		}
		if event.Type == "agent/inbox/spliced" && json.Valid(event.Data) {
			var data struct {
				Outcome string `json:"outcome"`
			}
			_ = json.Unmarshal(event.Data, &data)
			if data.Outcome == "canceled" {
				canceled++
			}
		}
	}
	if human != 1 || goalAdmitted != 0 || canceled != 1 {
		t.Fatalf("human=%d goal=%d canceled=%d", human, goalAdmitted, canceled)
	}
}

func TestNativePlanModeAppliesAtStepAndSurvivesColdResume(t *testing.T) {
	ctx := context.Background()
	storage := &nativeMemoryStorage{}
	model := &observingModel{}
	factory := NativeFactory{
		Storage: storage, Builder: nativeTestBuilder{newContainerTestBuilder(t, t.TempDir())}, Model: model,
		Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help with the user's task.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2},
	}
	identity := NativeRuntimeIdentity{UserID: "alice", SessionID: "native-plan", Workspace: "/workspace"}
	live, err := factory.Activate(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Plan.Queue(true); err != nil {
		t.Fatal(err)
	}
	if mode, err := live.Plan.View(); err != nil || mode.Active || mode.Pending == nil || !*mode.Pending {
		t.Fatalf("pending plan selection = %+v, %v", mode, err)
	}
	if _, err := live.Agent.RunTextTurn(ctx, "design first"); err != nil {
		t.Fatal(err)
	}
	if mode, err := live.Plan.View(); err != nil || !mode.Active || mode.Pending != nil {
		t.Fatalf("active plan = %+v, %v", mode, err)
	}
	if len(model.prompts) != 1 || !strings.Contains(model.prompts[0], plan.Policy) {
		t.Fatalf("plan policy absent from model request: %v", model.prompts)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := factory.Resume(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if mode, err := resumed.Plan.View(); err != nil || !mode.Active {
		t.Fatalf("cold plan state = %+v, %v", mode, err)
	}
	if err := resumed.Plan.SetNow(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Agent.RunTextTurn(ctx, "execute now"); err != nil {
		t.Fatal(err)
	}
	if len(model.prompts) != 2 || strings.Contains(model.prompts[1], plan.Policy) {
		t.Fatalf("inactive plan policy leaked into model request: %v", model.prompts)
	}
	var planEvents int
	for _, event := range resumed.Session.DurableEvents() {
		if event.Type == "plan/mode" {
			planEvents++
			if _, err := sessionformat.DecodePlanMode(event.Data); err != nil {
				t.Fatalf("invalid native plan/mode: %v", err)
			}
		}
	}
	if planEvents != 2 {
		t.Fatalf("durable plan/mode count = %d", planEvents)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_PLAN_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, nativeMemoryJSONL(t, storage), 0o600); err != nil {
			t.Fatalf("export native v4 plan journal: %v", err)
		}
	}
}

func TestNativeFactoryCreatesAndResumesChildSessionUnderRoot(t *testing.T) {
	ctx := context.Background()
	storage := &nativeTreeStorage{nativeMemoryStorage: &nativeMemoryStorage{}}
	model := &observingModel{}
	factory := NativeFactory{
		Storage: storage, Builder: nativeTestBuilder{newContainerTestBuilder(t, t.TempDir())}, Model: model,
		Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help with the user's task.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2}, Schedule: &schedule.Service{},
	}
	identity := NativeRuntimeIdentity{UserID: "alice", SessionID: "root-session", Workspace: "/workspace"}
	root, err := factory.Activate(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := root.Agent.RunTextTurn(ctx, "Set up the task"); err != nil {
		t.Fatal(err)
	}
	childFactory := factory
	childFactory.Provider = "child-provider"
	childFactory.ModelID = "child-model"
	childFactory.Model = agent.RoutedModel{Base: model, Route: agent.ModelRoute{Provider: "child-provider", Model: "child-model"}}
	child, err := childFactory.ActivateChild(ctx, identity, root, "child-session", "Research")
	if err != nil {
		t.Fatal(err)
	}
	if child.Session.Header().ParentSession != identity.SessionID || child.Session.Header().Origin != "subagent" || child.Session.Header().DelegationDepth != 1 {
		t.Fatalf("native child lineage = %+v", child.Session.Header())
	}
	if _, err := child.Agent.RunTextTurn(ctx, "Research this topic"); err != nil {
		t.Fatal(err)
	}
	clockSamples := func(events []sessionformat.Event) int {
		count := 0
		for _, event := range events {
			if event.Type == "user/message" && strings.Contains(string(event.Data), `"kind":"time-context"`) {
				count++
			}
		}
		return count
	}
	if rootCount, childCount := clockSamples(root.Session.DurableEvents()), clockSamples(child.Session.DurableEvents()); rootCount != 1 || childCount != 1 {
		t.Fatalf("standard clock context was not available to both Agents: root=%d child=%d", rootCount, childCount)
	}
	model.mu.Lock()
	rootSpecs, childSpecs := model.specs[0], model.specs[1]
	model.mu.Unlock()
	for _, name := range []string{"schedule_create", "schedule_list", "schedule_view", "schedule_edit"} {
		rootHas, childHas := false, false
		for _, spec := range rootSpecs {
			if spec.Name == name {
				rootHas = true
			}
		}
		for _, spec := range childSpecs {
			if spec.Name == name {
				childHas = true
			}
		}
		if !rootHas || !childHas {
			t.Fatalf("Schedule tool %s exposure: root=%v child=%v", name, rootHas, childHas)
		}
	}
	descriptor, err := sessionformat.FoldSubagentDescriptor(child.Session.DurableEvents())
	if err != nil || descriptor == nil || descriptor.Mode != "continuable" || descriptor.Label == nil || *descriptor.Label != "Research" ||
		descriptor.AgentProvider == nil || *descriptor.AgentProvider != "child-provider" || descriptor.AgentModel == nil || *descriptor.AgentModel != "child-model" {
		t.Fatalf("native child descriptor = %+v, %v", descriptor, err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := factory.ResumeChild(ctx, identity, identity.SessionID, "child-session")
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if _, err := resumed.Agent.RunTextTurn(ctx, "Continue the research"); err != nil {
		t.Fatal(err)
	}
	model.mu.Lock()
	routes := append([]agent.ModelRoute(nil), model.routes...)
	model.mu.Unlock()
	if len(routes) != 3 || routes[1].Provider != "child-provider" || routes[1].Model != "child-model" ||
		routes[2].Provider != "child-provider" || routes[2].Model != "child-model" {
		t.Fatalf("child route did not survive cold resume: %+v", routes)
	}
	var turns, descriptors int
	for _, event := range resumed.Session.DurableEvents() {
		if event.Type == "turn/end" {
			turns++
		}
		if event.Type == "subagent/descriptor" {
			descriptors++
		}
	}
	if turns != 2 || descriptors != 1 {
		t.Fatalf("native child turns=%d descriptors=%d", turns, descriptors)
	}
	if _, err := factory.ResumeChild(ctx, identity, "not-parent", "child-session"); err == nil {
		t.Fatal("child resumed under another parent")
	}
}

func TestNativeFactoryActivatesAndResumesWithRealBaseTools(t *testing.T) {
	ctx := context.Background()
	storage := &nativeMemoryStorage{}
	model := &observingModel{}
	factory := NativeFactory{
		Storage: storage, Builder: nativeTestBuilder{newContainerTestBuilder(t, t.TempDir())}, Model: model,
		Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help with the user's task.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2},
	}
	identity := NativeRuntimeIdentity{UserID: "alice", SessionID: "session-1", Workspace: "/workspace"}
	live, err := factory.Activate(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Agent.RunTextTurn(ctx, "first request"); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := factory.Resume(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Agent.RunTextTurn(ctx, "second request"); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	if storage.header.ID != identity.SessionID || storage.header.CWD != identity.Workspace || storage.header.Version != sessionformat.Version {
		t.Fatalf("incorrect native header: %+v", storage.header)
	}
	var turns, restores int
	for _, event := range storage.events {
		switch event.Type {
		case "turn/end":
			turns++
		case "session/end-seed":
			restores++
		}
	}
	if turns != 2 || restores != 1 {
		t.Fatalf("native session turns=%d restore markers=%d", turns, restores)
	}
	if len(model.specs) != 2 {
		t.Fatalf("model calls = %d", len(model.specs))
	}
	var names []string
	for _, spec := range model.specs[0] {
		names = append(names, spec.Name)
	}
	for _, name := range []string{"readfile", "writefile", "editfile", "shellexecute", "skill", "todo_write", "get_goal", "create_goal", "update_goal"} {
		if !slices.Contains(names, name) {
			t.Errorf("native base tool %q missing: %v", name, names)
		}
	}
	if slices.Contains(names, "publishartifacts") {
		t.Errorf("removed tool remains in native Assembly: %v", names)
	}
	if _, err := factory.Activate(ctx, identity); err == nil {
		t.Fatal("duplicate native activation was accepted")
	}
	if _, err := factory.Resume(ctx, NativeRuntimeIdentity{UserID: "alice", SessionID: "other", Workspace: "/workspace"}); err == nil {
		t.Fatal("native Session resumed under another identity")
	}
}
