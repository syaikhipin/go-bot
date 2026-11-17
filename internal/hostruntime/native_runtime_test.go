package hostruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/assembly"
	"go-bot/harness/goal"
	"go-bot/harness/interaction"
	"go-bot/harness/jobs"
	"go-bot/harness/plan"
	"go-bot/harness/session"
	"go-bot/harness/skills"
	"go-bot/harness/tools"
	"go-bot/internal/agentfs"
	"go-bot/internal/testsupport/session"
)

type nativeRuntimeStore struct {
	mu     sync.Mutex
	header sessionformat.Header
	events []sessionformat.Event
}

func nativeRuntimeJournalJSONL(t *testing.T, store *nativeRuntimeStore) []byte {
	t.Helper()
	store.mu.Lock()
	header := store.header
	events := append([]sessionformat.Event(nil), store.events...)
	store.mu.Unlock()
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

type nativeRuntimeJournal struct{ store *nativeRuntimeStore }

func (j nativeRuntimeJournal) Append(_ context.Context, events []sessionformat.Event) error {
	j.store.mu.Lock()
	defer j.store.mu.Unlock()
	if len(events) == 0 || len(j.store.events) != int(events[0].Seq) {
		return errors.New("native runtime journal position mismatch")
	}
	j.store.events = append(j.store.events, events...)
	return nil
}
func (nativeRuntimeJournal) Close() error { return nil }

func (s *nativeRuntimeStore) CreateNative(_ context.Context, header sessionformat.Header) (sessionformat.EventJournal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.header.ID != "" {
		return nil, errors.New("native runtime journal already exists")
	}
	s.header = header
	return nativeRuntimeJournal{s}, nil
}
func (s *nativeRuntimeStore) OpenNative(context.Context) (sessionformat.EventJournal, sessionformat.PhysicalArtifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.header.ID == "" {
		return nil, sessionformat.PhysicalArtifact{}, errors.New("native runtime journal missing")
	}
	artifact := sessionformat.PhysicalArtifact{Header: s.header, Events: append([]sessionformat.Event(nil), s.events...)}
	if s.header.IsSeeded {
		for _, event := range s.events {
			if event.Type == "session/end-seed" && string(event.Data) == `{"inherited":true}` {
				artifact.InheritedEventCount = event.Seq
			}
		}
	}
	return nativeRuntimeJournal{s}, artifact, nil
}

type nativeRuntimeBuilder struct{ workspace string }

func (b nativeRuntimeBuilder) BuildNative(_ context.Context, _ assembly.NativeRuntimeIdentity, _ string) (assembly.Resources, error) {
	return assembly.Resources{Files: agentfs.ContainerView{DefaultWorkdir: b.workspace}, Shell: nativeRuntimeShell{}, Skills: &skills.Registry{}, Close: func() error { return nil }}, nil
}

type nativeJobBuilder struct {
	nativeRuntimeBuilder
	registry *jobs.Registry
}

func TestNativeJobNoticeSummaryIsBounded(t *testing.T) {
	input := strings.Repeat("a", 121)
	if got := nativeBoundContextSummary(input); len([]rune(got)) != 120 || !strings.HasSuffix(got, "…") {
		t.Fatalf("unbounded Job notice summary: %q", got)
	}
	if got := nativeBoundContextSummary(input[:120]); got != input[:120] {
		t.Fatalf("exact-bound summary was changed: %q", got)
	}
}

func (b nativeJobBuilder) BuildNative(ctx context.Context, identity assembly.NativeRuntimeIdentity, cwd string) (assembly.Resources, error) {
	resources, err := b.nativeRuntimeBuilder.BuildNative(ctx, identity, cwd)
	if err != nil {
		return resources, err
	}
	resources.Jobs = b.registry
	closeSkills := resources.Close
	resources.Close = func() error {
		return errors.Join(b.registry.Close(context.Background()), closeSkills())
	}
	return resources, nil
}

type nativeJobModel struct{ notified chan struct{} }

func (m *nativeJobModel) Complete(_ context.Context, messages []agent.Message, _ []agent.ToolSpec) (session.AssistantMessage, error) {
	for _, message := range messages {
		if message.Role == "user" && strings.Contains(message.Content, "background job ") {
			select {
			case <-m.notified:
			default:
				close(m.notified)
			}
		}
	}
	return session.AssistantMessage{Content: "done"}, nil
}

type nativeJobGateModel struct {
	started  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	calls    int
	notified bool
}

func (m *nativeJobGateModel) Complete(ctx context.Context, messages []agent.Message, _ []agent.ToolSpec) (session.AssistantMessage, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	if call == 1 {
		close(m.started)
		select {
		case <-m.release:
		case <-ctx.Done():
			return session.AssistantMessage{}, ctx.Err()
		}
	}
	for _, message := range messages {
		if message.Role == "user" && strings.Contains(message.Content, "background job ") {
			m.mu.Lock()
			m.notified = true
			m.mu.Unlock()
		}
	}
	return session.AssistantMessage{Content: "done"}, nil
}

func TestNativeRuntimeBackgroundJobInjectsIntoActiveTurn(t *testing.T) {
	ctx := context.Background()
	registry, err := jobs.New(ctx, 2, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeJobGateModel{started: make(chan struct{}), release: make(chan struct{})}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, model)
	factory.Builder = nativeJobBuilder{nativeRuntimeBuilder: factory.Builder.(nativeRuntimeBuilder), registry: registry}
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "native-job-inject", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.SubmitText(ctx, "Start work"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first model call did not start")
	}
	job, err := registry.Start("shell", "quick task", func(context.Context, func([]byte)) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MarkVisible(job.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		worker.mu.Lock()
		injected := len(worker.live.Session.Inbox().NextStep) == 1
		worker.mu.Unlock()
		if injected {
			break
		}
		select {
		case <-deadline:
			t.Fatal("settled Job did not enter the active turn's next-step inbox")
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(model.release)
	waitNativeTurn(t, worker, 1)
	model.mu.Lock()
	calls, notified := model.calls, model.notified
	model.mu.Unlock()
	if calls != 2 || !notified {
		t.Fatalf("active turn model calls=%d notice seen=%t", calls, notified)
	}
	journal := nativeRuntimeJournalJSONL(t, factory.Storage.(*nativeRuntimeStore))
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(journal)); err != nil {
		t.Fatalf("active Job notice produced invalid v4 Session: %v", err)
	}
}

func TestNativeRuntimeBackgroundJobWakesIdleOwnerAndPreventsIdleReap(t *testing.T) {
	ctx := context.Background()
	registry, err := jobs.New(ctx, 2, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeJobModel{notified: make(chan struct{})}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, model)
	factory.Builder = nativeJobBuilder{nativeRuntimeBuilder: factory.Builder.(nativeRuntimeBuilder), registry: registry}
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "native-job", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.SubmitText(ctx, "Run a background task"); err != nil {
		t.Fatal(err)
	}
	waitNativeTurn(t, worker, 1)
	release := make(chan struct{})
	job, err := registry.Start("shell", "slow task", func(context.Context, func([]byte)) (int, error) {
		<-release
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MarkVisible(job.ID); err != nil {
		t.Fatal(err)
	}
	activity, err := worker.Activity()
	if err != nil || activity.Idle {
		t.Fatalf("running job activity = %+v, %v", activity, err)
	}
	close(release)
	waitNativeTurn(t, worker, 2)
	select {
	case <-model.notified:
	default:
		t.Fatal("Job completion did not reach the next model call")
	}
	journal := nativeRuntimeJournalJSONL(t, factory.Storage.(*nativeRuntimeStore))
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(journal)); err != nil {
		t.Fatalf("Job notice produced invalid v4 Session: %v", err)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_JOB_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, journal, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Contains(journal, []byte(`"kind":"tool-jobs"`)) || !bytes.Contains(journal, []byte(`"form":"notice"`)) {
		t.Fatal("v4 Session omitted producer-owned Job notice")
	}
}

type nativeRuntimeShell struct{}

func (nativeRuntimeShell) Run(context.Context, string, string, int) (tools.ShellResult, error) {
	return tools.ShellResult{}, errors.New("unexpected native Shell call")
}

type nativeRuntimeModel struct{}

func (nativeRuntimeModel) Complete(context.Context, []agent.Message, []agent.ToolSpec) (session.AssistantMessage, error) {
	return session.AssistantMessage{Content: "done"}, nil
}

type nativeManualCompactGateModel struct {
	mu             sync.Mutex
	normal         int
	summaryStarted chan struct{}
	summaryRelease chan struct{}
	fourthStarted  chan struct{}
	fourthRelease  chan struct{}
}

func (m *nativeManualCompactGateModel) Complete(ctx context.Context, messages []agent.Message, _ []agent.ToolSpec) (session.AssistantMessage, error) {
	if len(messages) > 0 && strings.Contains(messages[len(messages)-1].Content, "You are now acting as a compaction engine") {
		close(m.summaryStarted)
		select {
		case <-m.summaryRelease:
			return session.AssistantMessage{Content: "## Primary Request and Intent\n- Continue the work."}, nil
		case <-ctx.Done():
			return session.AssistantMessage{}, ctx.Err()
		}
	}
	m.mu.Lock()
	m.normal++
	number := m.normal
	m.mu.Unlock()
	if number == 4 {
		close(m.fourthStarted)
		select {
		case <-m.fourthRelease:
		case <-ctx.Done():
			return session.AssistantMessage{}, ctx.Err()
		}
	}
	return session.AssistantMessage{Content: "done"}, nil
}

func TestNativeRuntimeManualCompactionDelaysQueuedPrompt(t *testing.T) {
	ctx := context.Background()
	store := &nativeRuntimeStore{}
	model := &nativeManualCompactGateModel{
		summaryStarted: make(chan struct{}), summaryRelease: make(chan struct{}),
		fourthStarted: make(chan struct{}), fourthRelease: make(chan struct{}),
	}
	worker := &NativeRuntime{Factory: nativeRuntimeTestFactory(t, store, model),
		Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "manual-compact", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	var releaseSummary, releaseFourth sync.Once
	defer func() {
		releaseSummary.Do(func() { close(model.summaryRelease) })
		releaseFourth.Do(func() { close(model.fourthRelease) })
		worker.Abort()
		_ = worker.Close()
	}()
	for index := 0; index < 3; index++ {
		if _, err := worker.SubmitText(ctx, strings.Repeat(string(rune('a'+index)), 1600)); err != nil {
			t.Fatal(err)
		}
		waitNativeTurn(t, worker, index+1)
	}
	deadline := time.After(15 * time.Second)
	for {
		activity, err := worker.Activity()
		if err != nil {
			t.Fatal(err)
		}
		if activity.Idle {
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker did not settle after seed turns")
		case <-time.After(10 * time.Millisecond):
		}
	}
	result := make(chan error, 1)
	go func() {
		compacted, err := worker.CompactNow(ctx)
		if err == nil && !compacted {
			err = errors.New("manual compaction had no useful range")
		}
		result <- err
	}()
	select {
	case <-model.summaryStarted:
	case err := <-result:
		t.Fatalf("manual compaction settled before summary started: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("manual summary did not start")
	}
	if _, err := worker.CompactNow(ctx); !errors.Is(err, ErrNativeCompactionBusy) {
		t.Fatalf("parallel compaction = %v", err)
	}
	if _, err := worker.SubmitText(ctx, "after manual compaction"); err != nil {
		t.Fatal(err)
	}
	events, err := worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	turnStarts := 0
	for _, event := range events {
		if event.Type == "turn/start" {
			turnStarts++
		}
	}
	if turnStarts != 3 {
		t.Fatalf("queued prompt started before summary settled: %d turns", turnStarts)
	}
	releaseSummary.Do(func() { close(model.summaryRelease) })
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manual compaction did not settle")
	}
	select {
	case <-model.fourthStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("queued prompt did not start after compaction")
	}
	if _, err := worker.CompactNow(ctx); !errors.Is(err, ErrNativeCompactionBusy) {
		t.Fatalf("active-turn compaction = %v", err)
	}
	releaseFourth.Do(func() { close(model.fourthRelease) })
	waitNativeTurn(t, worker, 4)
	events, err = worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	var compactEnd, fourthStart uint64
	turnStarts = 0
	for _, event := range events {
		if event.Type == "compaction/end" {
			compactEnd = event.Seq
		}
		if event.Type == "turn/start" {
			turnStarts++
			if turnStarts == 4 {
				fourthStart = event.Seq
			}
		}
	}
	if compactEnd == 0 || fourthStart <= compactEnd {
		t.Fatalf("queued turn started before durable compaction end: end=%d turn=%d", compactEnd, fourthStart)
	}
	if err := sessionformat.ValidateV4Relationships(events); err != nil {
		t.Fatal(err)
	}
}

type nativeQuestionModel struct {
	calls     int
	allowSkip bool
	timed     bool
}

func (m *nativeQuestionModel) Complete(_ context.Context, messages []agent.Message, _ []agent.ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	if m.calls == 1 {
		arguments := `{"questions":[{"id":"choice","question":"Which option?","options":[{"label":"A"},{"label":"B"}]}]}`
		if m.timed {
			arguments = `{"questions":[{"id":"choice","question":"Which option?","options":[{"label":"A"},{"label":"B"}]}],"timeout":1}`
		}
		return session.AssistantMessage{ToolCalls: []session.ToolRequest{{
			ID: "question-call", Name: "ask_user_question",
			Arguments: arguments,
		}}}, nil
	}
	for _, message := range messages {
		if message.Role == "tool" && message.ToolName == "ask_user_question" && strings.Contains(message.Content, `"selected":["B"]`) {
			return session.AssistantMessage{Content: "User selected B."}, nil
		}
		if message.Role == "user" && strings.Contains(message.Content, `"kind":"answer_to_pending_question"`) &&
			strings.Contains(message.Content, `"selected":["B"]`) {
			return session.AssistantMessage{Content: "User selected B after the timeout."}, nil
		}
		if m.allowSkip && message.Role == "tool" && message.ToolName == "ask_user_question" &&
			strings.Contains(message.Content, `"selected":[]`) {
			return session.AssistantMessage{Content: "User skipped the question."}, nil
		}
	}
	if m.timed {
		return session.AssistantMessage{Content: "Continuing independent work while the question remains pending."}, nil
	}
	return session.AssistantMessage{}, errors.New("native question answer was absent from model context")
}

func TestNativeRuntimeTimedQuestionContinuesAndLateAnswerIsUserMessage(t *testing.T) {
	ctx := context.Background()
	store := &nativeRuntimeStore{}
	worker := &NativeRuntime{
		Factory:  nativeRuntimeTestFactory(t, store, &nativeQuestionModel{timed: true}),
		Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "native-timed-question", Workspace: "/workspace"},
	}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.SubmitText(ctx, "Ask and continue"); err != nil {
		t.Fatal(err)
	}
	waitNativeTurn(t, worker, 1)
	pending, err := worker.PendingQuestions()
	if err != nil || len(pending) != 1 || pending[0].State != "continued" {
		t.Fatalf("timed question did not continue: %+v, %v", pending, err)
	}
	events, err := worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	var pendingResult bool
	for _, event := range events {
		pendingResult = pendingResult || event.Type == "tool/result" && strings.Contains(string(event.Data), `\"pending\":true`)
	}
	if !pendingResult {
		t.Fatal("timed question did not record a pending tool result")
	}
	if export := os.Getenv("SERVERAGENTS_V4_TIMED_QUESTION_PENDING_EXPORT_PATH"); export != "" {
		if err := os.WriteFile(export, nativeRuntimeJournalJSONL(t, store), 0600); err != nil {
			t.Fatal(err)
		}
	}
	answer := interaction.Answer{Answers: []interaction.AnswerItem{{ID: "choice", Selected: []string{"B"}}}}
	if err := worker.AnswerQuestion(ctx, "question-call", answer); err != nil {
		t.Fatal(err)
	}
	if err := worker.AnswerQuestion(ctx, "question-call", answer); !errors.Is(err, interaction.ErrAlreadyAnswered) {
		t.Fatalf("duplicate late answer = %v", err)
	}
	waitNativeTurn(t, worker, 2)
	if pending, err := worker.PendingQuestions(); err != nil || len(pending) != 0 {
		t.Fatalf("late answer did not settle question: %+v, %v", pending, err)
	}
	events, err = worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	var reply bool
	for _, event := range events {
		reply = reply || event.Type == "user/message" && strings.Contains(string(event.Data), `"kind":"user-question-reply"`)
	}
	if !reply || sessionformat.ValidateV4Relationships(events) != nil {
		t.Fatal("late answer was not admitted as a valid v4 user message")
	}
	if export := os.Getenv("SERVERAGENTS_V4_TIMED_QUESTION_ANSWERED_EXPORT_PATH"); export != "" {
		if err := os.WriteFile(export, nativeRuntimeJournalJSONL(t, store), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeRuntimeTimedQuestionSurvivesColdResume(t *testing.T) {
	ctx := context.Background()
	store := &nativeRuntimeStore{}
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "native-timed-resume", Workspace: "/workspace"}
	factory := nativeRuntimeTestFactory(t, store, &nativeQuestionModel{timed: true})
	first := &NativeRuntime{Factory: factory, Identity: identity}
	if err := first.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := first.SubmitText(ctx, "Ask and continue"); err != nil {
		t.Fatal(err)
	}
	waitNativeTurn(t, first, 1)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	resumed := &NativeRuntime{Factory: factory, Identity: identity}
	if err := resumed.Start(ctx, true); err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if pending, err := resumed.PendingQuestions(); err != nil || len(pending) != 1 || pending[0].State != "continued" {
		t.Fatalf("timed question lost across resume: %+v, %v", pending, err)
	}
	if err := resumed.AnswerQuestion(ctx, "question-call", interaction.Answer{Answers: []interaction.AnswerItem{{ID: "choice", Selected: []string{"B"}}}}); err != nil {
		t.Fatal(err)
	}
	waitNativeTurn(t, resumed, 2)
	if pending, err := resumed.PendingQuestions(); err != nil || len(pending) != 0 {
		t.Fatalf("resumed timed question did not settle: %+v, %v", pending, err)
	}
}

type nativePlanReviewModel struct{ calls int }

func (m *nativePlanReviewModel) Complete(_ context.Context, messages []agent.Message, _ []agent.ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	if len(messages) == 0 {
		return session.AssistantMessage{}, errors.New("native plan request has no system prompt")
	}
	if m.calls == 1 {
		if !strings.Contains(messages[0].Content, plan.Policy) {
			return session.AssistantMessage{}, errors.New("native plan policy missing before review")
		}
		return session.AssistantMessage{ToolCalls: []session.ToolRequest{{
			ID: "plan-review-call", Name: "exit_plan_mode", Arguments: `{"plan":"# Report plan\n- Finish the report."}`,
		}}}, nil
	}
	if strings.Contains(messages[0].Content, plan.Policy) {
		return session.AssistantMessage{}, errors.New("native plan policy remained after approval")
	}
	return session.AssistantMessage{Content: "Plan approved; proceeding."}, nil
}

func TestNativeRuntimePlanReviewAppliesApprovedExitAtNextStep(t *testing.T) {
	ctx := context.Background()
	worker := &NativeRuntime{
		Factory:  nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, &nativePlanReviewModel{}),
		Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "native-plan-review", Workspace: "/workspace"},
	}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if view, err := worker.SetPlan(ctx, true); err != nil || !view.Active {
		t.Fatalf("set native plan = %+v, %v", view, err)
	}
	if _, err := worker.SubmitText(ctx, "Design first"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		pending, err := worker.PendingQuestions()
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 1 {
			if pending[0].CallID != "plan-review-call" || pending[0].Questions[0].Detail != "# Report plan\n- Finish the report." {
				t.Fatalf("native plan review = %+v", pending)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("native plan review did not open: %v", worker.LastError())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := worker.AnswerQuestion(ctx, "plan-review-call", interaction.Answer{Answers: []interaction.AnswerItem{{ID: "plan-review", Selected: []string{"Approve"}}}}); err != nil {
		t.Fatal(err)
	}
	waitNativeTurn(t, worker, 1)
	view, err := worker.GetPlan()
	if err != nil || view.Active {
		t.Fatalf("native plan did not exit after approval: %+v, %v", view, err)
	}
	page, err := worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	var flips int
	for _, event := range page {
		if event.Type == "plan/mode" {
			flips++
		}
		if strings.HasPrefix(event.Type, "question/") {
			t.Fatalf("plan review wrote old custom event %s", event.Type)
		}
	}
	if flips != 2 {
		t.Fatalf("native plan mode flips = %d", flips)
	}
}

func TestNativeRuntimeQuestionAnswerUsesToolResultNotCustomEvents(t *testing.T) {
	ctx := context.Background()
	store := &nativeRuntimeStore{}
	worker := &NativeRuntime{
		Factory:  nativeRuntimeTestFactory(t, store, &nativeQuestionModel{}),
		Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "native-question", Workspace: "/workspace"},
	}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.SubmitText(ctx, "Ask me first"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		pending, err := worker.PendingQuestions()
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 1 {
			if pending[0].CallID != "question-call" {
				t.Fatalf("pending native question = %+v", pending)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("native question was not presented: %v", worker.LastError())
		case <-time.After(10 * time.Millisecond):
		}
	}
	waitingEvents, err := worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	var waitingCall bool
	for _, event := range waitingEvents {
		if event.Type == "tool/call" && strings.Contains(string(event.Data), "question-call") {
			waitingCall = true
		}
		if event.Type == "tool/result" || event.Type == "turn/end" {
			t.Fatalf("waiting for a user answer already settled %s", event.Type)
		}
	}
	if !waitingCall {
		t.Fatal("pending user question has no durable tool/call")
	}
	if err := worker.AnswerQuestion(ctx, "question-call", interaction.Answer{Answers: []interaction.AnswerItem{{ID: "choice", Selected: []string{"B"}}}}); err != nil {
		t.Fatal(err)
	}
	waitNativeTurn(t, worker, 1)
	page, err := worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	var call, result, completed bool
	for _, event := range page {
		if strings.HasPrefix(event.Type, "question/") {
			t.Fatalf("native question wrote old custom event %q", event.Type)
		}
		if event.Type == "tool/call" && strings.Contains(string(event.Data), "question-call") {
			call = true
		}
		if event.Type == "tool/result" && strings.Contains(string(event.Data), `"selected\":[\"B\"]`) {
			result = true
		}
		if event.Type == "turn/end" && strings.Contains(string(event.Data), `"kind":"completed"`) {
			completed = true
		}
	}
	if !call || !result || !completed {
		t.Fatalf("native question call=%t result=%t completed=%t", call, result, completed)
	}
}

func TestNativeRuntimeInterruptedTimedQuestionAcceptsLateAnswerAfterColdResume(t *testing.T) {
	ctx := context.Background()
	store := &nativeRuntimeStore{}
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "interrupted-question", Workspace: "/workspace"}
	factory := nativeRuntimeTestFactory(t, store, &nativeQuestionModel{})
	first := &NativeRuntime{Factory: factory, Identity: identity}
	if err := first.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := first.SubmitText(ctx, "Ask me first"); err != nil {
		t.Fatal(err)
	}
	nativeWaitFor(t, func() bool {
		pending, err := first.PendingQuestions()
		return err == nil && len(pending) == 1 && pending[0].CallID == "question-call"
	})
	// Preserve the exact durable prefix a crashed Host leaves behind: the
	// question tool/call exists, but neither its result nor turn/end does.
	store.mu.Lock()
	crashPrefix := append([]sessionformat.Event(nil), store.events...)
	store.mu.Unlock()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.events = crashPrefix
	store.mu.Unlock()

	resumed := &NativeRuntime{Factory: factory, Identity: identity}
	if err := resumed.Start(ctx, true); err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	waitNativeTurn(t, resumed, 1)
	if pending, err := resumed.PendingQuestions(); err != nil || len(pending) != 1 || pending[0].State != "continued" {
		t.Fatalf("cold resume did not retain the timed question: %+v, %v", pending, err)
	}
	if err := resumed.AnswerQuestion(ctx, "question-call", interaction.Answer{Answers: []interaction.AnswerItem{{ID: "choice", Selected: []string{"B"}}}}); err != nil {
		t.Fatalf("late answer after interrupted wait = %v", err)
	}
	waitNativeTurn(t, resumed, 2)
	if pending, err := resumed.PendingQuestions(); err != nil || len(pending) != 0 {
		t.Fatalf("answered question remained pending: %+v, %v", pending, err)
	}
	events, err := resumed.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionformat.ValidateV4Relationships(events); err != nil {
		t.Fatalf("repaired question turn is not valid v4: %v", err)
	}
	var calls, results, endings int
	var lateAnswer bool
	for _, event := range events {
		switch event.Type {
		case "tool/call":
			calls++
		case "tool/result":
			results++
			if !strings.Contains(string(event.Data), `"code":"TOOL_OUTCOME_UNKNOWN"`) ||
				strings.Contains(string(event.Data), `"selected":["B"]`) {
				t.Fatalf("interrupted wait was presented as an answer: %s", event.Data)
			}
		case "turn/end":
			endings++
		case "user/message":
			lateAnswer = lateAnswer || strings.Contains(string(event.Data), `"kind":"user-question-reply"`)
		}
	}
	if calls != 1 || results != 1 || endings != 2 || !lateAnswer {
		t.Fatalf("interrupted question repaired as calls=%d results=%d endings=%d late=%t", calls, results, endings, lateAnswer)
	}
}

func TestNativeRuntimeSkippedQuestionReturnsEmptySelectionInV4(t *testing.T) {
	ctx := context.Background()
	worker := &NativeRuntime{
		Factory: nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, &nativeQuestionModel{allowSkip: true}),
		Identity: assembly.NativeRuntimeIdentity{
			UserID: "alice", SessionID: "native-question-skip", Workspace: "/workspace",
		},
	}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.SubmitText(ctx, "Ask me first"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		pending, err := worker.PendingQuestions()
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("question was not presented: %v", worker.LastError())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := worker.AnswerQuestion(ctx, "question-call", interaction.Answer{Answers: []interaction.AnswerItem{{ID: "choice", Selected: []string{}}}}); err != nil {
		t.Fatal(err)
	}
	waitNativeTurn(t, worker, 1)
	page, err := worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	var result, completed bool
	for _, event := range page {
		if event.Type == "tool/result" {
			var data struct {
				Message struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			result = len(data.Message.Content) == 1 &&
				data.Message.Content[0].Text == `{"answers":[{"id":"choice","selected":[]}]}`
		}
		if event.Type == "turn/end" && strings.Contains(string(event.Data), `"kind":"completed"`) {
			completed = true
		}
	}
	if !result || !completed {
		t.Fatalf("skipped question not persisted as a completed v4 turn: result=%t completed=%t", result, completed)
	}
	if err := sessionformat.ValidateV4Relationships(page); err != nil {
		t.Fatalf("skipped question v4 relationships: %v", err)
	}
}

type nativeOneRoundGoalModel struct{ calls int }

func (m *nativeOneRoundGoalModel) Complete(context.Context, []agent.Message, []agent.ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	if m.calls == 1 {
		return session.AssistantMessage{ToolCalls: []session.ToolRequest{{
			ID: "create-goal", Name: "create_goal", Arguments: `{"objective":"Finish the report","max_goal_rounds":1}`,
		}}}, nil
	}
	return session.AssistantMessage{Content: "Still working"}, nil
}

func TestNativeRuntimeAutomaticallyAdmitsOneGoalRoundThenBlocksAtLimit(t *testing.T) {
	ctx := context.Background()
	store := &nativeRuntimeStore{}
	worker := &NativeRuntime{
		Factory:  nativeRuntimeTestFactory(t, store, &nativeOneRoundGoalModel{}),
		Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "one-goal-round", Workspace: "/workspace"},
	}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.SubmitText(ctx, "Please finish this long-running report"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		view, found, err := worker.live.Goals.Get()
		if err != nil {
			t.Fatal(err)
		}
		if found && view.Goal.Phase == "blocked" {
			if view.RoundsStarted != 1 || view.Goal.BlockedReason == nil || view.Goal.BlockedReason.Code != "round-limit" {
				t.Fatalf("limit state = %+v", view)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("goal did not finish one automatic round: view=%+v found=%t last=%v", view, found, worker.LastError())
		case <-time.After(10 * time.Millisecond):
		}
	}
	page, err := worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	var goalSources, turns int
	for _, event := range page {
		if event.Type == "turn/end" {
			turns++
		}
		if event.Type == "user/message" && strings.Contains(string(event.Data), `"kind":"goal"`) {
			goalSources++
		}
	}
	if turns != 2 || goalSources != 1 {
		t.Fatalf("goal rounds=%d turns=%d", goalSources, turns)
	}
}

type nativeGateModel struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *nativeGateModel) Complete(ctx context.Context, _ []agent.Message, _ []agent.ToolSpec) (session.AssistantMessage, error) {
	m.once.Do(func() { close(m.started) })
	select {
	case <-m.release:
		return session.AssistantMessage{Content: "done"}, nil
	case <-ctx.Done():
		return session.AssistantMessage{}, ctx.Err()
	}
}

func nativeRuntimeTestFactory(t *testing.T, store *nativeRuntimeStore, model agent.Model) assembly.NativeFactory {
	t.Helper()
	return assembly.NativeFactory{
		Storage: store, Builder: nativeRuntimeBuilder{workspace: t.TempDir()}, Model: model,
		Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help the user.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2, MaxTurnDuration: 5 * time.Second},
	}
}

func TestNativeRuntimeResumeGoalWakesIdleWorker(t *testing.T) {
	ctx := context.Background()
	store := &nativeRuntimeStore{}
	worker := &NativeRuntime{
		Factory:  nativeRuntimeTestFactory(t, store, nativeRuntimeModel{}),
		Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "native-goal-resume", Workspace: "/workspace"},
	}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	created, err := worker.live.Goals.Create(ctx, "Finish the request", 1)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := worker.live.Goals.Pause(ctx, sessionformat.GoalRef{ID: created.Goal.ID, Revision: created.Goal.Revision})
	if err != nil {
		t.Fatal(err)
	}
	current, found, err := worker.GetGoal()
	if err != nil || !found || current.Goal.Phase != "paused" {
		t.Fatalf("paused native goal = %+v, %v, %v", current, found, err)
	}
	if _, err := worker.ResumeGoal(ctx, sessionformat.GoalRef{ID: paused.Goal.ID, Revision: paused.Goal.Revision - 1}); !errors.Is(err, goal.ErrStaleRevision) {
		t.Fatalf("stale resume = %v", err)
	}
	resumed, err := worker.ResumeGoal(ctx, sessionformat.GoalRef{ID: paused.Goal.ID, Revision: paused.Goal.Revision})
	if err != nil || resumed.Activation != "armed" {
		t.Fatalf("resume native goal = %+v, %v", resumed, err)
	}
	waitNativeTurn(t, worker, 1)
	page, err := worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	var goalInput bool
	for _, event := range page {
		if event.Type == "agent/inbox/spliced" && strings.Contains(string(event.Data), `"kind":"goal"`) {
			goalInput = true
		}
	}
	if !goalInput {
		t.Fatal("resumed idle goal did not enqueue its continuation")
	}
}

func TestNativeRuntimeStopDeadlineCoversChildDrainAndCanRetry(t *testing.T) {
	worker := &NativeRuntime{
		Factory:  nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeRuntimeModel{}),
		Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "native-child-stop-deadline", Workspace: t.TempDir()},
	}
	if err := worker.Start(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	worker.mu.Lock()
	worker.children = newNativeChildManager(worker.Factory, worker.Identity, worker.live, worker.kick)
	worker.mu.Unlock()
	worker.children.mu.Lock()
	worker.children.children["blocked"] = &nativeChildProcess{
		cancel: func() {
			select {
			case cancelled <- struct{}{}:
			default:
			}
		},
		done: blocked,
	}
	worker.children.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := worker.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stopping with a noncooperative child = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("child drain ignored the caller's Stop deadline")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("timed-out child drain did not cancel the child")
	}
	close(blocked)
	if err := worker.Stop(context.Background()); err != nil {
		t.Fatalf("retrying child drain after it settled: %v", err)
	}
}

func TestNativeChildManagerCloseDeadlineCoversPendingCreator(t *testing.T) {
	manager := &nativeChildManager{children: make(map[string]*nativeChildProcess)}
	manager.pending.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := manager.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending creator close deadline = %v", err)
	}
	manager.pending.Done()
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("retrying close after creator settled: %v", err)
	}
}

func TestNativeChildManagerKeepsClosingChildVisibleUntilResourcesClose(t *testing.T) {
	session, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "closing-child", CreatedAt: 1}, nativeRuntimeJournal{})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	live := &assembly.NativeLive{Session: session, Resource: assembly.Resources{Close: func() error {
		close(entered)
		<-release
		return nil
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	process := &nativeChildProcess{live: live, cancel: func() {}, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	manager := &nativeChildManager{children: map[string]*nativeChildProcess{"closing-child": process}}
	cancel()
	go manager.driveChild(ctx, nil, process)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("child did not enter resource cleanup")
	}
	deadline, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if err := manager.Close(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("manager missed child still closing resources: %v", err)
	}
	close(release)
	<-process.done
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("manager did not finish after child resource close: %v", err)
	}
}

func TestNativeRuntimeDrivesDurableInboxAndColdResume(t *testing.T) {
	ctx := context.Background()
	storage := &nativeRuntimeStore{}
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "session-1", Workspace: "/workspace"}
	factory := assembly.NativeFactory{
		Storage: storage, Builder: nativeRuntimeBuilder{workspace: t.TempDir()}, Model: nativeRuntimeModel{},
		Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help the user.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2, MaxTurnDuration: 5 * time.Second},
	}
	worker := &NativeRuntime{Factory: factory, Identity: identity}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	firstID, err := worker.SubmitText(ctx, "first request")
	if err != nil || firstID == "" {
		t.Fatalf("first admission: %q, %v", firstID, err)
	}
	waitNativeTurn(t, worker, 1)
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.SubmitText(ctx, "late request"); err == nil {
		t.Fatal("stopped worker accepted another message")
	}
	resumed := &NativeRuntime{Factory: factory, Identity: identity}
	if err := resumed.Start(ctx, true); err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	secondID, err := resumed.SubmitText(ctx, "second request")
	if err != nil || secondID == "" || secondID == firstID {
		t.Fatalf("resumed admission: %q, %v", secondID, err)
	}
	waitNativeTurn(t, resumed, 2)
	page, err := resumed.DurableEventsAfter(nil, 500)
	if err != nil || len(page) == 0 || page[0].Seq != 0 {
		t.Fatalf("native first page omitted seq 0: %d events, %v", len(page), err)
	}
	cursor := page[0].Seq
	next, err := resumed.DurableEventsAfter(&cursor, 500)
	if err != nil || len(next) != len(page)-1 || next[0].Seq != 1 {
		t.Fatalf("native cursor page = %d events, %v", len(next), err)
	}
}

func TestNativeRuntimeRepairsEmptyInboxInterruptedTurnAfterResume(t *testing.T) {
	ctx := context.Background()
	store := &nativeRuntimeStore{}
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "session-open-tail", Workspace: "/workspace"}
	factory := nativeRuntimeTestFactory(t, store, nativeRuntimeModel{})
	first := &NativeRuntime{Factory: factory, Identity: identity}
	if err := first.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.events = append(store.events, sessionformat.Event{
		Type: "turn/start", Seq: uint64(len(store.events)), Time: time.Now().UnixMilli(),
		Data: json.RawMessage(`{"turn":1}`),
	})
	store.mu.Unlock()
	resumed := &NativeRuntime{Factory: factory, Identity: identity}
	if err := resumed.Start(ctx, true); err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	waitNativeTurn(t, resumed, 1)
	activity, err := resumed.Activity()
	if err != nil || !activity.Idle {
		t.Fatalf("repaired activity = %+v, %v", activity, err)
	}
}

func TestNativeRuntimeStopDrainsAlreadyAcceptedTurns(t *testing.T) {
	ctx := context.Background()
	worker := &NativeRuntime{
		Factory: assembly.NativeFactory{
			Storage: &nativeRuntimeStore{}, Builder: nativeRuntimeBuilder{workspace: t.TempDir()}, Model: nativeRuntimeModel{},
			Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help the user.",
			Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2, MaxTurnDuration: 5 * time.Second},
		},
		Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "session-stop", Workspace: "/workspace"},
	}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.SubmitText(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.SubmitText(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := worker.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if err := worker.Stop(stopCtx); err != nil {
		t.Fatalf("second stop should be safe: %v", err)
	}
	page, err := worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	var completed int
	for _, event := range page {
		if event.Type == "turn/end" {
			completed++
		}
	}
	if completed != 2 {
		t.Fatalf("graceful stop abandoned %d accepted turns: %d events", 2-completed, len(page))
	}
	if _, err := worker.SubmitText(ctx, "third"); err == nil {
		t.Fatal("native worker accepted input after graceful stop")
	}
}

func TestNativeRuntimeGracefulStopWaitsForRunningTurnThenDrainsQueue(t *testing.T) {
	ctx := context.Background()
	model := &nativeGateModel{started: make(chan struct{}), release: make(chan struct{})}
	worker := &NativeRuntime{
		Factory:  nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, model),
		Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "session-graceful", Workspace: "/workspace"},
	}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.SubmitText(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("native model did not start")
	}
	if _, err := worker.SubmitText(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- worker.Stop(ctx) }()
	select {
	case err := <-stopped:
		t.Fatalf("graceful stop returned during an active turn: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(model.release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("graceful stop did not finish after model release")
	}
	waitNativeTurn(t, worker, 2)
}

func TestNativeRuntimeAbortLeavesQueuedMessageForColdResume(t *testing.T) {
	ctx := context.Background()
	store := &nativeRuntimeStore{}
	model := &nativeGateModel{started: make(chan struct{}), release: make(chan struct{})}
	factory := nativeRuntimeTestFactory(t, store, model)
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "session-abort", Workspace: "/workspace"}
	worker := &NativeRuntime{Factory: factory, Identity: identity}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.SubmitText(ctx, "running"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("native model did not start")
	}
	if activity, err := worker.Activity(); err != nil || activity.Idle {
		t.Fatalf("running model appeared idle: %+v, %v", activity, err)
	}
	if _, err := worker.SubmitText(ctx, "queued"); err != nil {
		t.Fatal(err)
	}
	worker.Abort()
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	_, artifact, err := store.OpenNative(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := sessionformat.FoldInbox(artifact.Events)
	if err != nil || len(inbox.NextTurn) != 1 {
		t.Fatalf("aborted worker lost queued input: %+v, %v", inbox, err)
	}
	for _, event := range artifact.Events {
		if event.Type == "turn/end" && !strings.Contains(string(event.Data), `"kind":"disposed"`) {
			t.Fatalf("owner abort reason = %s", event.Data)
		}
	}
	resumed := &NativeRuntime{Factory: nativeRuntimeTestFactory(t, store, nativeRuntimeModel{}), Identity: identity}
	if err := resumed.Start(ctx, true); err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	waitNativeTurn(t, resumed, 2) // the aborted turn and the queued resumed turn
}

func TestNativeRuntimeCancelTurnDurablyDropsPendingInbox(t *testing.T) {
	ctx := context.Background()
	model := &nativeGateModel{started: make(chan struct{}), release: make(chan struct{})}
	store := &nativeRuntimeStore{}
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "session-cancel", Workspace: "/workspace"}
	worker := &NativeRuntime{Factory: nativeRuntimeTestFactory(t, store, model), Identity: identity}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.SubmitText(ctx, "running request"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(3 * time.Second):
		t.Fatal("native model did not start")
	}
	if _, err := worker.SubmitText(ctx, "queued request"); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.CancelTurn(ctx, "unknown"); !errors.Is(err, ErrNativeTurnNotFound) {
		t.Fatalf("unknown turn cancellation = %v", err)
	}
	state, err := worker.CancelTurn(ctx, "1")
	if err != nil || state != "running" {
		t.Fatalf("running turn cancellation = %q, %v", state, err)
	}
	waitNativeTurn(t, worker, 1)
	if _, err := worker.CancelTurn(ctx, "1"); !errors.Is(err, ErrNativeTurnFinished) {
		t.Fatalf("finished turn cancellation = %v", err)
	}
	page, err := worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionformat.ValidateV4Relationships(page); err != nil {
		t.Fatalf("cancelled v4 Session relationships: %v", err)
	}
	inbox, err := sessionformat.FoldInbox(page)
	if err != nil || len(inbox.NextStep) != 0 || len(inbox.NextTurn) != 0 {
		t.Fatalf("cancelled inbox = %+v, %v", inbox, err)
	}
	var cancellations, turnStarts int
	for _, event := range page {
		switch event.Type {
		case "agent/inbox/spliced":
			var data struct {
				Outcome string `json:"outcome"`
			}
			if json.Unmarshal(event.Data, &data) == nil && data.Outcome == "canceled" {
				cancellations++
			}
		case "turn/start":
			turnStarts++
		case "turn/end":
			if !strings.Contains(string(event.Data), `"kind":"aborted"`) || !strings.Contains(string(event.Data), `"kind":"user"`) {
				t.Fatalf("user cancellation reason = %s", event.Data)
			}
		}
	}
	if cancellations != 1 || turnStarts != 1 {
		t.Fatalf("cancelled queued work = %d splices, %d starts", cancellations, turnStarts)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_WORKER_CANCEL_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, nativeRuntimeJournalJSONL(t, store), 0o600); err != nil {
			t.Fatalf("export canceled worker v4 journal: %v", err)
		}
	}
}

func TestNativeRuntimeTurnDeadlineIsNotReportedAsUserCancel(t *testing.T) {
	ctx := context.Background()
	model := &nativeGateModel{started: make(chan struct{}), release: make(chan struct{})}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, model)
	factory.Limits.MaxTurnDuration = 50 * time.Millisecond
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "session-timeout", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.SubmitText(ctx, "slow request"); err != nil {
		t.Fatal(err)
	}
	waitNativeTurn(t, worker, 1)
	page, err := worker.DurableEventsAfter(nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page {
		if event.Type == "turn/end" {
			if !strings.Contains(string(event.Data), `"kind":"hook"`) || !strings.Contains(string(event.Data), "turn deadline exceeded") {
				t.Fatalf("timeout cancellation reason = %s", event.Data)
			}
			return
		}
	}
	t.Fatal("turn deadline did not end the v4 turn")
}

func waitNativeTurn(t *testing.T, worker *NativeRuntime, target int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		events, err := worker.DurableEventsAfter(nil, 500)
		if err != nil {
			t.Fatal(err)
		}
		completed := 0
		for _, event := range events {
			if event.Type == "turn/end" {
				completed++
			}
		}
		if completed >= target {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("native worker completed %d/%d turns: last error %v", completed, target, worker.LastError())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
