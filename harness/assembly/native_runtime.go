package assembly

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/attachments"
	"go-bot/harness/goal"
	"go-bot/harness/interaction"
	"go-bot/harness/memory"
	"go-bot/harness/permission"
	"go-bot/harness/plan"
	"go-bot/harness/schedule"
	"go-bot/harness/session"
	"go-bot/harness/sessionsearch"
	"go-bot/harness/skills"
	"go-bot/harness/tools"
)

// NativeRuntimeIdentity stays outside DeepSeek's Session header. User owner
// and workspace are Host deployment facts, not model-surface events.
type NativeRuntimeIdentity struct {
	UserID    string
	SessionID string
	Workspace string
	// CWD is this Agent's initial/current directory inside the shared worker
	// filesystem. Empty uses Workspace; child Sessions persist their own CWD.
	CWD               string
	AgentPreset       string
	PermissionCeiling func() permission.Mode
}

type NativeStorage interface {
	CreateNative(context.Context, sessionformat.Header) (sessionformat.EventJournal, error)
	OpenNative(context.Context) (sessionformat.EventJournal, sessionformat.PhysicalArtifact, error)
}

type NativeChildStorageFactory interface {
	ChildNativeStorage(string) (NativeStorage, error)
}

type NativeBuilder interface {
	BuildNative(context.Context, NativeRuntimeIdentity, string) (Resources, error)
}

// NativeFactory assembles one v4 Session and Agent inside the user Host.
type NativeFactory struct {
	Storage          NativeStorage
	Builder          NativeBuilder
	Model            agent.Model
	ResolveTurnModel func(context.Context) (agent.TurnModel, error)
	Provider         string
	ModelID          string
	ReasoningEffort  string
	SystemPrompt     string
	Limits           agent.Limits
	Children         tools.ChildCoordinator
	Schedule         *schedule.Service
	FileAttachments  attachments.FileStore
	Memory           *memory.Store
	SessionSearch    *sessionsearch.Index
	// WorkflowSchema is an activation-only child contract. It is not persisted
	// and must not leak into ordinary or resumed child Sessions.
	WorkflowSchema map[string]any
	// ToolApproval may request an explicit pre-dispatch decision for a tool.
	// An empty request lets normal execution proceed.
	ToolApproval func(context.Context, string, json.RawMessage) *permission.Request
}

type NativeLive struct {
	Agent      *agent.NativeLoop
	Session    *sessionformat.LiveSession
	Goals      *goal.NativeService
	Plan       *plan.NativeService
	Questions  *interaction.NativeQuestions
	Resource   Resources
	Structured *tools.NativeStructuredOutput
	closeMu    sync.Once
	closeErr   error
}

func (l *NativeLive) Close() error {
	l.closeMu.Do(func() {
		if l.Questions != nil {
			l.Questions.Close()
		}
		l.closeErr = errors.Join(l.Session.Close(), l.Resource.close())
	})
	return l.closeErr
}

func (f NativeFactory) validate(identity NativeRuntimeIdentity) error {
	if f.Storage == nil || f.Builder == nil || f.Model == nil || f.Provider == "" || f.ModelID == "" ||
		f.SystemPrompt == "" || f.Limits.MaxSteps <= 0 || f.Limits.MaxToolCalls <= 0 ||
		identity.UserID == "" || identity.SessionID == "" || !filepath.IsAbs(identity.Workspace) ||
		len(identity.Workspace) > 4096 {
		return errors.New("incomplete native container Assembly")
	}
	if err := f.Limits.ValidateContextWindow(); err != nil {
		return err
	}
	if !filepath.IsAbs(nativeCWD(identity)) || len(nativeCWD(identity)) > 4096 {
		return errors.New("native Agent cwd must be an absolute container path")
	}
	if f.Limits.MaxModelCalls < 0 || f.Limits.MaxTurnDuration < 0 || f.Limits.MaxTurnDuration > 24*time.Hour {
		return errors.New("invalid native model-call or turn-duration limit")
	}
	return nil
}

func nativeCWD(identity NativeRuntimeIdentity) string {
	if identity.CWD != "" {
		return filepath.Clean(identity.CWD)
	}
	return identity.Workspace
}

func (f NativeFactory) Activate(ctx context.Context, identity NativeRuntimeIdentity) (*NativeLive, error) {
	if err := f.validate(identity); err != nil {
		return nil, err
	}
	header := sessionformat.Header{
		Type: "session", Version: sessionformat.Version,
		ID: identity.SessionID, CreatedAt: time.Now().UTC().UnixMilli(), CWD: nativeCWD(identity), AgentPreset: identity.AgentPreset,
	}
	journal, err := f.Storage.CreateNative(ctx, header)
	if err != nil {
		return nil, err
	}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		_ = journal.Close()
		return nil, err
	}
	if err := permission.Save(ctx, s, permission.FullAccess, false, true); err != nil {
		_ = s.Close()
		return nil, err
	}
	return f.publish(ctx, identity, s, "activation", nil)
}

func (f NativeFactory) Resume(ctx context.Context, identity NativeRuntimeIdentity) (*NativeLive, error) {
	if err := f.validate(identity); err != nil {
		return nil, err
	}
	journal, artifact, err := f.Storage.OpenNative(ctx)
	if err != nil {
		return nil, err
	}
	if artifact.Header.ID != identity.SessionID || artifact.Header.CWD != nativeCWD(identity) || artifact.Header.Origin == "subagent" {
		_ = journal.Close()
		return nil, errors.New("native Session header differs from its worker identity")
	}
	s, err := sessionformat.RestoreLiveSession(artifact, journal)
	if err != nil {
		_ = journal.Close()
		return nil, err
	}
	if err := s.Flush(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return f.publish(ctx, identity, s, "resume", nil)
}

// ActivateChild creates a separate child Session inside the same Host. It has
// its own v4 journal in the shared file tree, but no new container or listener.
func (f NativeFactory) ActivateChild(ctx context.Context, root NativeRuntimeIdentity, parent *NativeLive, childID, label string) (*NativeLive, error) {
	return f.ActivateChildAt(ctx, root, parent, childID, label, "")
}

func (f NativeFactory) ActivateChildAt(ctx context.Context, root NativeRuntimeIdentity, parent *NativeLive, childID, label, cwd string) (*NativeLive, error) {
	return f.activateChild(ctx, root, parent, childID, label, cwd, nil)
}

// ForkChild captures only the parent's last fully closed turn. The copied
// prefix is stored in the child's own v4 journal, so cold resume never reads
// the parent's newer history to reconstruct this child.
func (f NativeFactory) ForkChild(ctx context.Context, root NativeRuntimeIdentity, parent *NativeLive, childID, label string) (*NativeLive, error) {
	return f.ForkChildAt(ctx, root, parent, childID, label, "")
}

func (f NativeFactory) ForkChildAt(ctx context.Context, root NativeRuntimeIdentity, parent *NativeLive, childID, label, cwd string) (*NativeLive, error) {
	if parent == nil {
		return nil, errors.New("native fork requires a live parent")
	}
	events := parent.Session.DurableEvents()
	cut := -1
	for index, event := range events {
		if event.Type == "turn/end" {
			cut = index
		}
	}
	if cut < 0 {
		return f.activateChild(ctx, root, parent, childID, label, cwd, nil)
	}
	return f.activateChild(ctx, root, parent, childID, label, cwd, events[:cut+1])
}

func (f NativeFactory) activateChild(ctx context.Context, root NativeRuntimeIdentity, parent *NativeLive, childID, label, cwd string, seed []sessionformat.Event) (*NativeLive, error) {
	if parent == nil || childID == "" || childID == parent.Session.Header().ID || label == "" {
		return nil, errors.New("invalid native child identity")
	}
	storage, ok := f.Storage.(NativeChildStorageFactory)
	if !ok {
		return nil, errors.New("native child Session storage is unavailable")
	}
	childStorage, err := storage.ChildNativeStorage(childID)
	if err != nil {
		return nil, err
	}
	childFactory := f
	childFactory.Storage = childStorage
	identity := root
	identity.SessionID = childID
	identity.PermissionCeiling = func() permission.Mode { return permission.Read(parent.Session.Events()).Mode }
	parentCWD := nativeSessionCWD(parent.Session.Header().CWD, parent.Session.Events())
	if cwd == "" {
		cwd = parentCWD
	} else if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(parentCWD, cwd)
	}
	identity.CWD = filepath.Clean(cwd)
	if err := childFactory.validate(identity); err != nil {
		return nil, err
	}
	parentHeader := parent.Session.Header()
	header := sessionformat.Header{
		Type: "session", Version: sessionformat.Version, ID: childID,
		CreatedAt: time.Now().UTC().UnixMilli(), CWD: parentHeader.CWD,
		ParentSession: parentHeader.ID, Origin: "subagent", IsSeeded: len(seed) > 0,
		DelegationDepth: parentHeader.DelegationDepth + 1,
		AgentPreset:     parentHeader.AgentPreset,
	}
	journal, err := childStorage.CreateNative(ctx, header)
	if err != nil {
		return nil, err
	}
	var s *sessionformat.LiveSession
	if len(seed) == 0 {
		s, err = sessionformat.NewLiveSession(header, journal)
	} else {
		prefix := append([]sessionformat.Event(nil), seed...)
		marker := sessionformat.Event{
			Type: "session/end-seed", Seq: uint64(len(prefix)), Time: prefix[len(prefix)-1].Time,
			Data: json.RawMessage(`{"inherited":true}`),
		}
		prefix = append(prefix, marker)
		artifact := sessionformat.PhysicalArtifact{Header: header, Events: prefix, InheritedEventCount: marker.Seq}
		s, err = sessionformat.RestoreLiveSession(artifact, journal)
		if err == nil {
			err = journal.Append(ctx, prefix)
		}
	}
	if err != nil {
		_ = journal.Close()
		return nil, err
	}
	if err := permission.Save(ctx, s, permission.Read(parent.Session.Events()).Mode, true, true); err != nil {
		_ = s.Close()
		return nil, err
	}
	if identity.CWD != header.CWD {
		data, err := json.Marshal(struct {
			CWD string `json:"cwd"`
		}{CWD: identity.CWD})
		if err != nil {
			_ = s.Close()
			return nil, err
		}
		if _, err := s.Append(sessionformat.EventInput{Type: "working-directory/change", Data: data}); err != nil {
			_ = s.Close()
			return nil, err
		}
		if err := s.Flush(ctx); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	descriptor := &sessionformat.SubagentDescriptor{
		Version: 3, Mode: "continuable", Provider: "native", Label: &label,
		AgentProvider: &f.Provider, AgentModel: &f.ModelID,
	}
	if f.ReasoningEffort != "" {
		descriptor.AgentReasoningEffort = &f.ReasoningEffort
	}
	return childFactory.publish(ctx, identity, s, "activation", descriptor)
}

// ResumeChild reopens only a v4 child already cataloged by its live direct
// parent. The caller verifies adjacency; the durable header and first
// descriptor must then agree with that claim.
func (f NativeFactory) ResumeChild(ctx context.Context, root NativeRuntimeIdentity, parentID, childID string) (*NativeLive, error) {
	storage, ok := f.Storage.(NativeChildStorageFactory)
	if !ok || childID == "" || parentID == "" {
		return nil, errors.New("native child Session storage is unavailable")
	}
	childStorage, err := storage.ChildNativeStorage(childID)
	if err != nil {
		return nil, err
	}
	childFactory := f
	childFactory.Storage = childStorage
	identity := root
	identity.SessionID = childID
	journal, artifact, err := childStorage.OpenNative(ctx)
	if err != nil {
		return nil, err
	}
	identity.CWD = nativeSessionCWD(artifact.Header.CWD, artifact.Events)
	if err := childFactory.validate(identity); err != nil {
		_ = journal.Close()
		return nil, err
	}
	if artifact.Header.ID != childID || artifact.Header.ParentSession != parentID ||
		artifact.Header.Origin != "subagent" || !filepath.IsAbs(artifact.Header.CWD) || artifact.Header.DelegationDepth < 1 {
		_ = journal.Close()
		return nil, errors.New("native child lineage differs from its parent")
	}
	descriptor, err := sessionformat.FoldSubagentDescriptor(artifact.Events)
	if err != nil || descriptor == nil || descriptor.Mode != "continuable" || descriptor.Provider != "native" {
		_ = journal.Close()
		return nil, errors.New("native child lacks a resumable current descriptor")
	}
	if descriptor.AgentProvider != nil {
		childFactory.Provider = *descriptor.AgentProvider
	}
	if descriptor.AgentModel != nil {
		childFactory.ModelID = *descriptor.AgentModel
	}
	if descriptor.AgentReasoningEffort != nil {
		childFactory.ReasoningEffort = *descriptor.AgentReasoningEffort
	}
	if childFactory.Provider != f.Provider || childFactory.ModelID != f.ModelID || childFactory.ReasoningEffort != f.ReasoningEffort {
		childFactory.Model = agent.RoutedModel{Base: f.Model, Route: agent.ModelRoute{
			Provider: childFactory.Provider, Model: childFactory.ModelID, ReasoningEffort: childFactory.ReasoningEffort,
		}}
	}
	s, err := sessionformat.RestoreLiveSession(artifact, journal)
	if err != nil {
		_ = journal.Close()
		return nil, err
	}
	if err := s.Flush(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return childFactory.publish(ctx, identity, s, "resume", nil)
}

// DeepSeek keeps the original project cwd in the immutable Session header;
// later working-directory/change events determine the effective cwd.
func nativeSessionCWD(initial string, events []sessionformat.Event) string {
	current := initial
	for _, event := range events {
		if event.Type != "working-directory/change" {
			continue
		}
		var change struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(event.Data, &change) == nil && filepath.IsAbs(change.CWD) {
			current = filepath.Clean(change.CWD)
		}
	}
	return current
}

func (f NativeFactory) publish(ctx context.Context, identity NativeRuntimeIdentity, s *sessionformat.LiveSession, reason string, descriptor *sessionformat.SubagentDescriptor) (*NativeLive, error) {
	resource, err := f.Builder.BuildNative(ctx, identity, reason)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	if resource.Files == nil || resource.Shell == nil || resource.Skills == nil {
		_ = s.Close()
		_ = resource.close()
		return nil, errors.New("native container Builder omitted base capabilities")
	}
	toolset, err := baseTools(resource)
	if err != nil {
		_ = s.Close()
		_ = resource.close()
		return nil, err
	}
	skillLookup := func() skills.Lookup {
		return skills.Lookup{CWD: nativeSessionCWD(s.Header().CWD, s.Events()), Workspace: identity.Workspace}
	}
	skillTool := &tools.Skill{Registry: resource.Skills, Lookup: skillLookup}
	toolset = append(toolset, skillTool, tools.Present{
		Workspace: identity.Workspace,
		CWD:       func() string { return skillLookup().CWD },
	})
	goals, err := goal.NewNative(s)
	if err != nil {
		_ = s.Close()
		_ = resource.close()
		return nil, err
	}
	plans, err := plan.NewNative(s)
	if err != nil {
		_ = s.Close()
		_ = resource.close()
		return nil, err
	}
	questions := interaction.NewNativeQuestions()
	approval := permission.ApprovalService{Session: s, Questions: questions}
	permissionState := func() permission.State {
		state := permission.Read(s.Events())
		if identity.PermissionCeiling != nil {
			ceiling := identity.PermissionCeiling()
			if ceiling.Rank() < state.Mode.Rank() {
				state.Mode = ceiling
			}
			state.Approval = "never"
		}
		return state
	}
	permissionRoot := s.Header().CWD
	if resource.ConfineMutation != nil {
		for i, tool := range toolset {
			switch tool.Spec().Name {
			case "shellexecute", "writefile", "editfile":
				toolset[i] = tools.PermissionTool{Tool: tool, State: permissionState, Root: permissionRoot,
					Approvals: approval, ConfineMutation: resource.ConfineMutation}
			}
		}
	}
	toolset = append(toolset, tools.NativeTodoWrite{Session: s, AllowParallelInProgress: true})
	toolset = append(toolset,
		tools.NativeGetGoal{Goals: goals, Session: s},
		tools.NativeCreateGoal{Goals: goals, Session: s},
		tools.NativeUpdateGoal{Goals: goals, Session: s},
		tools.NativeAskUserQuestion{Questions: questions},
		tools.NativeExitPlanMode{Plan: plans, Questions: questions},
	)
	if f.Schedule != nil {
		for _, name := range []string{"schedule_create", "schedule_list", "schedule_view", "schedule_edit"} {
			toolset = append(toolset, tools.NativeSchedule{Service: f.Schedule, SessionID: identity.SessionID,
				Name: name, Delegated: s.Header().Origin == "subagent",
				CWD: s.Header().CWD, AgentPreset: s.Header().AgentPreset})
		}
	}
	if f.Children != nil {
		toolset = append(toolset,
			tools.Subagent{Coordinator: f.Children, ParentID: identity.SessionID},
			tools.Subagent{Coordinator: f.Children, ParentID: identity.SessionID, Fork: true},
			tools.SendMessage{Coordinator: f.Children, SenderID: identity.SessionID},
			tools.InterruptAgent{Coordinator: f.Children, ParentID: identity.SessionID},
			tools.ListAgents{Coordinator: f.Children, ParentID: identity.SessionID},
		)
		if coordinator, ok := f.Children.(tools.WorkflowCoordinator); ok {
			toolset = append(toolset, tools.NativeWorkflow{
				Coordinator: coordinator, ParentID: identity.SessionID, Session: s, Jobs: resource.Jobs,
			})
		}
	}
	toolset = append(toolset, resource.ExtraTools...)
	if f.Memory != nil {
		toolset = append(toolset, tools.NativeMemory{Store: f.Memory})
	}
	if f.SessionSearch != nil {
		toolset = append(toolset, tools.NativeSessionSearch{Index: f.SessionSearch, SessionID: identity.SessionID})
	}
	var structured *tools.NativeStructuredOutput
	if f.WorkflowSchema != nil {
		if s.Header().Origin != "subagent" {
			_ = s.Close()
			_ = resource.close()
			return nil, errors.New("structured output is limited to child activation")
		}
		structured, err = tools.NewNativeStructuredOutput(f.WorkflowSchema)
		if err != nil {
			_ = s.Close()
			_ = resource.close()
			return nil, err
		}
		toolset = append(toolset, structured)
	}
	cwd := resource.DefaultWorkdir
	if cwd == "" {
		cwd = nativeCWD(identity)
	}
	basePrompt := composePrompt(f.SystemPrompt, toolset, cwd)
	if f.Memory != nil {
		basePrompt += "\n\n" + memory.Instruction
	}
	if f.SessionSearch != nil {
		basePrompt += "\n\nIf the user refers to an earlier conversation, or relevant context may be in a previous session, look it up with session_search before asking the user to repeat it."
	}
	if structured != nil {
		basePrompt += "\n\n" + tools.NativeStructuredOutputInstruction
	}
	loop, err := agent.NewNativeLoop(s, f.Model, f.Provider, f.ModelID, basePrompt, toolset,
		f.Limits.MaxSteps, f.Limits.MaxToolCalls)
	if err != nil {
		_ = s.Close()
		_ = resource.close()
		return nil, fmt.Errorf("assemble native Agent loop: %w", err)
	}
	loop.SetReasoningEffort(f.ReasoningEffort)
	loop.SetTurnModelResolver(f.ResolveTurnModel)
	// The clock is standard-preset context for every Agent, including a child;
	// only the four Schedule management tools are root-only.
	if f.Schedule != nil {
		if err := loop.EnableTimeContext("UTC", 10*time.Minute); err != nil {
			_ = s.Close()
			_ = resource.close()
			return nil, err
		}
	}
	if err := loop.SetModelCallLimit(f.Limits.MaxModelCalls); err != nil {
		_ = s.Close()
		_ = resource.close()
		return nil, err
	}
	if err := loop.SetParallelToolLimit(f.Limits.MaxParallelToolCalls); err != nil {
		_ = s.Close()
		_ = resource.close()
		return nil, err
	}
	if err := loop.SetContextPressure(f.Limits.ContextWindowTokens, f.Limits.ReservedOutputTokens, f.Limits.ContextHeadroomTokens); err != nil {
		_ = s.Close()
		_ = resource.close()
		return nil, err
	}
	loop.SetPreStepHook(func(stepCtx context.Context) (agent.NativePreStepDecision, error) {
		if descriptor != nil {
			data, err := json.Marshal(descriptor)
			if err != nil {
				return agent.NativePreStepEnter, err
			}
			if _, err := s.Append(sessionformat.EventInput{Type: "subagent/descriptor", Data: data}); err != nil {
				return agent.NativePreStepEnter, err
			}
			descriptor = nil
			if err := s.Flush(stepCtx); err != nil {
				return agent.NativePreStepEnter, err
			}
		}
		return agent.NativePreStepEnter, plans.ApplyPending(stepCtx)
	})
	if resource.ConfineMutation != nil {
		loop.SetRuntimeContext(func() []agent.RuntimeContextSection {
			file, approval := permission.Guidance(permissionState(), permissionRoot)
			return []agent.RuntimeContextSection{
				{Name: "working-directory:current", Text: fmt.Sprintf("Current working directory: %q.", skillLookup().CWD)},
				{Name: "sandbox:policy", Text: file},
				{Name: "approval:policy", Text: approval},
			}
		})
	}

	if f.ToolApproval != nil {
		loop.SetToolApproval(func(ctx context.Context, name string, args json.RawMessage) error {
			request := f.ToolApproval(ctx, name, args)
			if request == nil {
				return nil
			}
			action := *request
			action.Tool = name
			return approval.Request(ctx, action)
		})
	}

	loop.SetSkillCatalogProvider(skillTool, func(stepCtx context.Context) (agent.NativeSkillCatalogSnapshot, error) {
		discovery, err := resource.Skills.Snapshot(stepCtx, skillLookup())
		if err != nil {
			return agent.NativeSkillCatalogSnapshot{}, err
		}
		snapshot := agent.NativeSkillCatalogSnapshot{Complete: discovery.Complete}
		for _, entry := range discovery.Entries {
			if entry.ModelInvocable {
				snapshot.Entries = append(snapshot.Entries, agent.NativeSkillCatalogEntry{
					Name: entry.Name, Description: entry.Description,
				})
			}
		}
		return snapshot, nil
	})
	loop.SetSkillInvocationLoader(func(stepCtx context.Context, name string) (string, bool, error) {
		candidate, found, err := resource.Skills.Find(stepCtx, skillLookup(), name)
		if err != nil || !found {
			return "", false, err
		}
		loaded, found, err := candidate.Load(stepCtx)
		if err != nil || !found || !loaded.UserInvocable {
			return "", false, err
		}
		return tools.RenderSkillContent(loaded.Value), true, nil
	})
	loop.SetSystemPromptProvider(func() (string, error) {
		mode, err := plans.View()
		if err != nil {
			return "", err
		}
		prompt := basePrompt
		if mode.Active {
			prompt += "\n\n" + plan.Policy
		}
		if f.Memory != nil {
			notes, err := f.Memory.Prompt()
			if err != nil {
				slog.Warn("read account memory", "error", err)
			}
			prompt += notes
		}
		return prompt, nil
	})
	instructionReader := &nativeInstructionReader{}
	loop.SetInstructionProvider(func(stepCtx context.Context, touched []string) (agent.NativeInstructionSnapshot, error) {
		return instructionReader.loadCurrent(stepCtx, cwd, touched...)
	})
	loop.SetGoalClaimGuard(func(message json.RawMessage, inbox sessionformat.InboxState) (bool, error) {
		if len(inbox.NextTurn) != 1 || len(inbox.NextStep) != 0 {
			return false, nil // a competing accepted input takes precedence
		}
		var data struct {
			Source struct {
				Kind     string `json:"kind"`
				GoalID   string `json:"goalId"`
				Revision uint64 `json:"revision"`
				Round    uint64 `json:"round"`
			} `json:"source"`
		}
		if err := json.Unmarshal(message, &data); err != nil {
			return false, err
		}
		view, found, err := goals.Get()
		if err != nil || !found {
			return false, err
		}
		return data.Source.Kind == "goal" && view.Goal.ID == data.Source.GoalID &&
			view.Goal.Revision == data.Source.Revision && view.RoundsStarted+1 == data.Source.Round &&
			view.Goal.Phase == "active" && view.Activation == "armed" &&
			data.Source.Round <= view.Goal.MaxGoalRounds, nil
	})
	loop.SetFilePathResolver(func(ref agent.NativeFileAttachmentRef) string {
		return f.FileAttachments.Path(attachments.FileRef{ID: ref.ID, Name: ref.Name, Bytes: ref.Bytes})
	})
	return &NativeLive{Agent: loop, Session: s, Goals: goals, Plan: plans, Questions: questions, Resource: resource, Structured: structured}, nil
}
