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

	"go-harness/harness/agent"
	"go-harness/harness/assembly"
	"go-harness/harness/jobs"
	"go-harness/harness/session"
	"go-harness/harness/workflow"
	"go-harness/internal/testsupport/session"

	"github.com/google/uuid"
)

type nativeWorkflowToolModel struct{}

type nativeWorkflowParityModel struct{}

type nativePairedChildModel struct {
	entered chan struct{}
	release chan struct{}
}

func (m nativePairedChildModel) Complete(ctx context.Context, messages []agent.Message, _ []agent.ToolSpec) (session.AssistantMessage, error) {
	for _, message := range messages {
		if message.Role == "user" && strings.Contains(message.Content, "paired child task") {
			m.entered <- struct{}{}
			select {
			case <-m.release:
				return session.AssistantMessage{Content: "paired child finished"}, nil
			case <-ctx.Done():
				return session.AssistantMessage{}, ctx.Err()
			}
		}
	}
	return session.AssistantMessage{Content: "parent received settlement"}, nil
}

func TestNativeSiblingChildrenSettleTogetherWithoutLosingParentNotices(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	model := nativePairedChildModel{entered: make(chan struct{}, 2), release: make(chan struct{})}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, model)
	factory.Storage = rootStore
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{
		UserID: "alice", SessionID: "root", Workspace: "/root/harness/workspaces/root",
	}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			close(model.release)
		}
		_ = worker.Close()
	}()
	ids := make([]string, 0, 2)
	for range 2 {
		id, err := worker.SpawnChild(ctx, "root", "Parallel research", "paired child task", false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for range 2 {
		select {
		case <-model.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("both children did not reach the model")
		}
	}
	if activity, err := worker.Activity(); err != nil || activity.Idle {
		t.Fatalf("Host treated two progressing children as idle: %+v, %v", activity, err)
	}
	close(model.release)
	released = true
	for _, id := range ids {
		childStore := nativeTreeNode{tree: tree, id: id}
		nativeWaitFor(t, func() bool { return nativeTreeHasEvent(childStore, "turn/end") })
	}
	nativeWaitFor(t, func() bool {
		seen := make(map[string]bool)
		for _, event := range nativeTreeEvents(rootStore) {
			if event.Type != "agent/inbox/spliced" || !bytes.Contains(event.Data, []byte(`"kind":"subagent-settled"`)) {
				continue
			}
			for _, id := range ids {
				if bytes.Contains(event.Data, []byte(id)) {
					seen[id] = true
				}
			}
		}
		return len(seen) == len(ids)
	})
	nativeWaitFor(t, func() bool {
		activity, err := worker.Activity()
		return err == nil && activity.Idle && nativeTreeHasEvent(rootStore, "turn/end")
	})
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(nativeTreeJSONL(t, rootStore))); err != nil {
		t.Fatalf("parent v4 journal after concurrent child settlement: %v", err)
	}
}

func TestNativeListAgentsHidesOneShotHistoryAcrossResume(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeRuntimeModel{})
	factory.Storage = rootStore
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/root/harness/workspaces/root"}
	worker := &NativeRuntime{Factory: factory, Identity: identity}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	oneShotID := uuid.NewString()
	oneShot, err := json.Marshal(sessionformat.SubagentCatalogEntry{
		Version: 0, ChildID: oneShotID, ChildCreatedAt: 1, Mode: "one-shot",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.live.Session.Append(sessionformat.EventInput{Type: "subagent/catalog", Data: oneShot}); err != nil {
		t.Fatal(err)
	}
	if err := worker.live.Session.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	continuableID, err := worker.SpawnChild(ctx, "root", "Research", "Find an answer", false)
	if err != nil {
		t.Fatal(err)
	}
	check := func(runtime *NativeRuntime) {
		t.Helper()
		children, err := runtime.ListChildren(ctx, "root")
		if err != nil || len(children) != 1 || children[0].SessionID != continuableID || children[0].Label != "Research" {
			t.Fatalf("model-facing child list exposed one-shot history: %+v, %v", children, err)
		}
		if _, err := runtime.SendChildMessage(ctx, "root", oneShotID, "Continue"); err == nil {
			t.Fatal("one-shot child accepted a continuation message")
		}
	}
	check(worker)
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(nativeTreeNode{tree: tree, id: continuableID}, "turn/end") })
	if err := worker.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	resumed := &NativeRuntime{Factory: factory, Identity: identity}
	if err := resumed.Start(ctx, true); err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	check(resumed)
}

func (nativeWorkflowParityModel) Complete(ctx context.Context, messages []agent.Message, specs []agent.ToolSpec) (session.AssistantMessage, error) {
	structured := false
	for _, spec := range specs {
		structured = structured || spec.Name == "structured_output"
	}
	if structured {
		found := false
		workflowAvailable := false
		for _, spec := range specs {
			found = found || spec.Name == "structured_output"
			workflowAvailable = workflowAvailable || spec.Name == "workflow"
		}
		if !found {
			return session.AssistantMessage{}, errors.New("workflow child lacks structured_output")
		}
		if !workflowAvailable {
			return session.AssistantMessage{}, errors.New("workflow child lacks the standard workflow tool")
		}
		return session.AssistantMessage{ToolCalls: []session.ToolRequest{{ID: "structured-call", Name: "structured_output", Arguments: `{"answer":42}`}}}, nil
	}
	for _, message := range messages {
		if message.Role == "tool" && message.ToolName == "workflow" {
			if !strings.Contains(message.Content, `"answer": 42`) {
				return session.AssistantMessage{}, errors.New("structured workflow value did not reach parent")
			}
			return session.AssistantMessage{Content: "structured workflow complete"}, nil
		}
	}
	return session.AssistantMessage{ToolCalls: []session.ToolRequest{{
		ID: "workflow-structured", Name: "workflow",
		Arguments: `{"script":"return await agent('calculate', {schema:{type:'object',properties:{answer:{type:'integer'}},required:['answer'],additionalProperties:false}});","meta":{"name":"calculate","description":"Calculate a value"}}`,
	}}}, nil
}

func TestNativeHostWorkflowToolRoutesAndCapturesStructuredChild(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeWorkflowParityModel{})
	factory.Storage = rootStore
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.SubmitText(ctx, "Run the structured workflow"); err != nil {
		t.Fatal(err)
	}
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(rootStore, "turn/end") })
	if !strings.Contains(string(nativeTreeJSONL(t, rootStore)), "structured workflow complete") {
		t.Fatal("parent never received the structured workflow result")
	}
	entries, err := sessionformat.FoldSubagentCatalog(nativeTreeEvents(rootStore), 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("workflow child catalog = %+v, %v", entries, err)
	}
	childStore := nativeTreeNode{tree: tree, id: entries[0].ChildID}
	if !strings.Contains(string(nativeTreeJSONL(t, childStore)), `"model":"test-model"`) {
		t.Fatal("child v4 request header lost inherited main model")
	}
	if _, err := worker.RunWorkflowChild(ctx, entries[0].ChildID, "nested task", workflow.AgentOptions{}); err == nil || !strings.Contains(err.Error(), "not eligible") {
		t.Fatalf("depth-one workflow child started another delegated Agent: %v", err)
	}
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(nativeTreeJSONL(t, rootStore))); err != nil {
		t.Fatalf("parent v4 workflow log: %v", err)
	}
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(nativeTreeJSONL(t, childStore))); err != nil {
		t.Fatalf("child v4 workflow log: %v", err)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_WORKFLOW_CHILD_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, nativeTreeJSONL(t, childStore), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

type nativeStructuredChildModel struct{}

type nativePlainStructuredChildModel struct{}

func TestNativeSubagentInheritsMainAcrossColdResume(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeRuntimeModel{})
	factory.Storage = rootStore
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}
	worker := &NativeRuntime{Factory: factory, Identity: identity}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	id, err := worker.SpawnChildAt(ctx, "root", "Child", "Task", "", false)
	if err != nil {
		t.Fatal(err)
	}
	childStore := nativeTreeNode{tree: tree, id: id}
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(childStore, "turn/end") })
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	descriptor, err := sessionformat.FoldSubagentDescriptor(nativeTreeEvents(childStore))
	if err != nil || descriptor == nil || descriptor.AgentProvider == nil || *descriptor.AgentProvider != factory.Provider || descriptor.AgentModel == nil || *descriptor.AgentModel != factory.ModelID {
		t.Fatalf("child did not inherit main: %+v %v", descriptor, err)
	}
	resumed := &NativeRuntime{Factory: factory, Identity: identity}
	if err := resumed.Start(ctx, true); err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if _, err := resumed.SendChildMessage(ctx, "root", id, "Continue"); err != nil {
		t.Fatal(err)
	}
	nativeWaitFor(t, func() bool {
		n := 0
		for _, event := range nativeTreeEvents(childStore) {
			if event.Type == "turn/end" {
				n++
			}
		}
		return n >= 2
	})
}

type nativeResumeGate struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	opens   int
}

type nativeResumeGateStorage struct {
	nativeTreeNode
	gate *nativeResumeGate
}

func (s nativeResumeGateStorage) ChildNativeStorage(id string) (assembly.NativeStorage, error) {
	storage, err := s.nativeTreeNode.ChildNativeStorage(id)
	if err != nil {
		return nil, err
	}
	return nativeResumeGateChildStorage{NativeStorage: storage, gate: s.gate}, nil
}

type nativeResumeGateChildStorage struct {
	assembly.NativeStorage
	gate *nativeResumeGate
}

func (s nativeResumeGateChildStorage) OpenNative(ctx context.Context) (sessionformat.EventJournal, sessionformat.PhysicalArtifact, error) {
	s.gate.mu.Lock()
	s.gate.opens++
	first := s.gate.opens == 1
	s.gate.mu.Unlock()
	if first {
		close(s.gate.entered)
		<-s.gate.release // Simulate a storage operation that has not honored cancellation yet.
	}
	return s.NativeStorage.OpenNative(ctx)
}

func TestNativeChildColdResumeDoesNotHoldManagerLockOrBlockBoundedClose(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeRuntimeModel{})
	factory.Storage = rootStore
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}
	first := &NativeRuntime{Factory: factory, Identity: identity}
	if err := first.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	id, err := first.SpawnChild(ctx, "root", "Research", "Find an answer", false)
	if err != nil {
		t.Fatal(err)
	}
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(nativeTreeNode{tree: tree, id: id}, "turn/end") })
	if err := first.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	gate := &nativeResumeGate{entered: make(chan struct{}), release: make(chan struct{})}
	factory.Storage = nativeResumeGateStorage{nativeTreeNode: rootStore, gate: gate}
	resumed := &NativeRuntime{Factory: factory, Identity: identity}
	if err := resumed.Start(ctx, true); err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}()
	sent := make(chan error, 1)
	go func() {
		_, err := resumed.SendChildMessage(ctx, "root", id, "Continue")
		sent <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child cold resume did not reach storage")
	}
	listed := make(chan error, 1)
	go func() {
		_, err := resumed.ListChildren(ctx, "root")
		listed <- err
	}()
	select {
	case err := <-listed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked cold resume held the entire child manager lock")
	}
	stopCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if err := resumed.children.Close(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded close during cold resume = %v", err)
	}
	close(gate.release)
	select {
	case err := <-sent:
		if err == nil {
			t.Fatal("cold-resumed child accepted a message after manager shutdown")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cold resume did not release after storage resumed")
	}
	if err := resumed.children.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNativeChildConcurrentColdMessagesResumeOnce(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeRuntimeModel{})
	factory.Storage = rootStore
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}
	first := &NativeRuntime{Factory: factory, Identity: identity}
	if err := first.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	id, err := first.SpawnChild(ctx, "root", "Research", "Find an answer", false)
	if err != nil {
		t.Fatal(err)
	}
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(nativeTreeNode{tree: tree, id: id}, "turn/end") })
	if err := first.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	gate := &nativeResumeGate{entered: make(chan struct{}), release: make(chan struct{})}
	factory.Storage = nativeResumeGateStorage{nativeTreeNode: rootStore, gate: gate}
	resumed := &NativeRuntime{Factory: factory, Identity: identity}
	if err := resumed.Start(ctx, true); err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}()
	results := make(chan error, 2)
	for _, message := range []string{"First follow-up", "Second follow-up"} {
		go func(message string) {
			_, err := resumed.SendChildMessage(ctx, "root", id, message)
			results <- err
		}(message)
	}
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child cold resume did not reach storage")
	}
	nativeWaitFor(t, func() bool {
		resumed.children.mu.Lock()
		defer resumed.children.mu.Unlock()
		return resumed.children.deliveries[id] != nil && resumed.children.deliveries[id].refs == 2
	})
	close(gate.release)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent child message was not delivered")
		}
	}
	gate.mu.Lock()
	opens := gate.opens
	gate.mu.Unlock()
	if opens != 1 {
		t.Fatalf("same child was cold-resumed %d times", opens)
	}
}

type nativeChildCloseGateBuilder struct {
	base    assembly.NativeBuilder
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	armed   bool
	blocked bool
}

func (b *nativeChildCloseGateBuilder) BuildNative(ctx context.Context, identity assembly.NativeRuntimeIdentity, reason string) (assembly.Resources, error) {
	resources, err := b.base.BuildNative(ctx, identity, reason)
	if err != nil || identity.SessionID == "root" {
		return resources, err
	}
	previous := resources.Close
	resources.Close = func() error {
		b.mu.Lock()
		block := b.armed && !b.blocked
		if block {
			b.blocked = true
		}
		b.mu.Unlock()
		if block {
			close(b.entered)
			<-b.release
		}
		return previous()
	}
	return resources, nil
}

func TestNativeChildMessageWaitsForInterruptedChildThenColdResumes(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeRuntimeModel{})
	factory.Storage = rootStore
	gate := &nativeChildCloseGateBuilder{base: factory.Builder, entered: make(chan struct{}), release: make(chan struct{})}
	factory.Builder = gate
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}()
	id, err := worker.SpawnChild(ctx, "root", "Research", "Find an answer", false)
	if err != nil {
		t.Fatal(err)
	}
	childStore := nativeTreeNode{tree: tree, id: id}
	nativeWaitFor(t, func() bool {
		if !nativeTreeHasEvent(childStore, "turn/end") {
			return false
		}
		children, err := worker.ListChildren(ctx, "root")
		return err == nil && len(children) == 1 && children[0].Activity == "idle"
	})
	gate.mu.Lock()
	gate.armed = true
	gate.mu.Unlock()
	if state, err := worker.InterruptChild(ctx, "root", id); err != nil || state != "stopping" {
		t.Fatalf("interrupt child = %q, %v", state, err)
	}
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted child did not reach resource close")
	}
	if activity, err := worker.Activity(); err != nil || activity.Idle {
		t.Fatalf("Host considered a closing child idle: %+v, %v", activity, err)
	}
	sent := make(chan error, 1)
	go func() {
		_, err := worker.SendChildMessage(ctx, "root", id, "Continue after interruption")
		sent <- err
	}()
	select {
	case err := <-sent:
		t.Fatalf("message was submitted to a closing child: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate.release)
	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("message did not cold-resume the child: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message did not complete after child disposal")
	}
	nativeWaitFor(t, func() bool {
		ends := 0
		for _, event := range nativeTreeEvents(childStore) {
			if event.Type == "turn/end" {
				ends++
			}
		}
		return ends >= 2
	})
}

type nativeChildAppendGate struct {
	mu           sync.Mutex
	armed        bool
	blocked      bool
	entered      chan struct{}
	release      chan struct{}
	journalClose chan struct{}
	closeOnce    sync.Once
}

type nativeChildAppendGateStorage struct {
	nativeTreeNode
	gate *nativeChildAppendGate
}

func (s nativeChildAppendGateStorage) ChildNativeStorage(id string) (assembly.NativeStorage, error) {
	storage, err := s.nativeTreeNode.ChildNativeStorage(id)
	if err != nil {
		return nil, err
	}
	return nativeChildAppendGateNode{NativeStorage: storage, gate: s.gate}, nil
}

type nativeChildAppendGateNode struct {
	assembly.NativeStorage
	gate *nativeChildAppendGate
}

func (n nativeChildAppendGateNode) CreateNative(ctx context.Context, header sessionformat.Header) (sessionformat.EventJournal, error) {
	journal, err := n.NativeStorage.CreateNative(ctx, header)
	if err != nil {
		return nil, err
	}
	return nativeChildAppendGateJournal{EventJournal: journal, gate: n.gate}, nil
}

func (n nativeChildAppendGateNode) OpenNative(ctx context.Context) (sessionformat.EventJournal, sessionformat.PhysicalArtifact, error) {
	journal, artifact, err := n.NativeStorage.OpenNative(ctx)
	if err != nil {
		return nil, artifact, err
	}
	return nativeChildAppendGateJournal{EventJournal: journal, gate: n.gate}, artifact, nil
}

type nativeChildAppendGateJournal struct {
	sessionformat.EventJournal
	gate *nativeChildAppendGate
}

func (j nativeChildAppendGateJournal) Append(ctx context.Context, events []sessionformat.Event) error {
	for _, event := range events {
		if event.Type != "agent/inbox/spliced" {
			continue
		}
		j.gate.mu.Lock()
		block := j.gate.armed && !j.gate.blocked
		if block {
			j.gate.blocked = true
		}
		j.gate.mu.Unlock()
		if block {
			close(j.gate.entered)
			<-j.gate.release
		}
	}
	return j.EventJournal.Append(ctx, events)
}

func (j nativeChildAppendGateJournal) Close() error {
	j.gate.closeOnce.Do(func() { close(j.gate.journalClose) })
	return j.EventJournal.Close()
}

func TestNativeChildInterruptWaitsForAdmittedJournalAppend(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	gate := &nativeChildAppendGate{entered: make(chan struct{}), release: make(chan struct{}), journalClose: make(chan struct{})}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeRuntimeModel{})
	factory.Storage = nativeChildAppendGateStorage{nativeTreeNode: rootStore, gate: gate}
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}()
	id, err := worker.SpawnChild(ctx, "root", "Research", "Find an answer", false)
	if err != nil {
		t.Fatal(err)
	}
	childStore := nativeTreeNode{tree: tree, id: id}
	nativeWaitFor(t, func() bool {
		if !nativeTreeHasEvent(childStore, "turn/end") {
			return false
		}
		children, err := worker.ListChildren(ctx, "root")
		return err == nil && len(children) == 1 && children[0].Activity == "idle"
	})
	gate.mu.Lock()
	gate.armed = true
	gate.mu.Unlock()
	sent := make(chan error, 1)
	go func() {
		_, err := worker.SendChildMessage(ctx, "root", id, "Append before interrupt")
		sent <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child inbox append did not reach the journal")
	}
	if state, err := worker.InterruptChild(ctx, "root", id); err != nil || state != "stopping" {
		t.Fatalf("interrupt while child append is active = %q, %v", state, err)
	}
	select {
	case <-gate.journalClose:
		t.Fatal("child journal closed while an admitted append was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate.release)
	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("admitted append was lost on interrupt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admitted append did not finish")
	}
	select {
	case <-gate.journalClose:
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted child journal did not close after append")
	}
	if !bytes.Contains(nativeTreeJSONL(t, childStore), []byte("Append before interrupt")) {
		t.Fatal("the accepted child message was not durable")
	}
}

func (nativePlainStructuredChildModel) Complete(context.Context, []agent.Message, []agent.ToolSpec) (session.AssistantMessage, error) {
	return session.AssistantMessage{Content: `{"answer":42}`}, nil
}

func (nativeStructuredChildModel) Complete(_ context.Context, messages []agent.Message, specs []agent.ToolSpec) (session.AssistantMessage, error) {
	found := false
	for _, spec := range specs {
		if spec.Name == "structured_output" {
			found = true
			break
		}
	}
	if !found {
		return session.AssistantMessage{}, errors.New("structured child tool was not scoped to this activation")
	}
	for _, message := range messages {
		if message.Role == "tool" && message.ToolName == "structured_output" {
			if strings.Contains(message.Content, "Structured output recorded") {
				return session.AssistantMessage{}, errors.New("structured tool should have concluded the child turn")
			}
			return session.AssistantMessage{ToolCalls: []session.ToolRequest{{ID: "valid", Name: "structured_output", Arguments: `{"answer":42}`}}}, nil
		}
	}
	return session.AssistantMessage{ToolCalls: []session.ToolRequest{{ID: "invalid", Name: "structured_output", Arguments: `{"answer":"not-an-integer"}`}}}, nil
}

func TestNativeWorkflowStructuredChildCapturesOnlyValidatedToolResult(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeStructuredChildModel{})
	factory.Storage = rootStore
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	schema := map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer"}}, "required": []any{"answer"}, "additionalProperties": false}
	result, err := worker.RunWorkflowChild(ctx, "root", "Produce a structured answer", workflow.AgentOptions{Schema: schema})
	if err != nil || !result.Completed {
		t.Fatalf("structured workflow child = %+v, %v", result, err)
	}
	value, ok := result.Value.(map[string]any)
	if !ok || value["answer"] != float64(42) {
		t.Fatalf("structured capture = %#v", result.Value)
	}
	entries, err := sessionformat.FoldSubagentCatalog(nativeTreeEvents(rootStore), 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("structured child catalog = %+v, %v", entries, err)
	}
	childStore := nativeTreeNode{tree: tree, id: entries[0].ChildID}
	invalid, valid := false, false
	for _, event := range nativeTreeEvents(childStore) {
		if event.Type != "tool/result" {
			continue
		}
		invalid = invalid || strings.Contains(string(event.Data), "INVALID_ARGUMENTS")
		valid = valid || strings.Contains(string(event.Data), "Structured output recorded")
	}
	if !invalid || !valid {
		t.Fatal("child did not durably reject invalid schema and capture corrected output")
	}
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(nativeTreeJSONL(t, childStore))); err != nil {
		t.Fatalf("structured child v4 log: %v", err)
	}
}

func TestNativeWorkflowStructuredChildDoesNotAcceptPlainJSONText(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativePlainStructuredChildModel{})
	factory.Storage = nativeTreeNode{tree: tree, id: "root"}
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	schema := map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer"}}}
	result, err := worker.RunWorkflowChild(ctx, "root", "Produce a structured answer", workflow.AgentOptions{Schema: schema})
	if err != nil || result.Completed || result.Value != nil {
		t.Fatalf("plain text was treated as structured output: %+v, %v", result, err)
	}
}

func TestNativeWorkflowRejectsModelOverridesBeforeCreatingChild(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeRuntimeModel{})
	factory.Storage = rootStore
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.RunWorkflowChild(ctx, "root", "Task", workflow.AgentOptions{Model: "another-model"}); err == nil {
		t.Fatal("child model override accepted")
	}
	entries, err := sessionformat.FoldSubagentCatalog(nativeTreeEvents(rootStore), 0)
	if err != nil || len(entries) != 0 {
		t.Fatal("rejected override left a child session")
	}
}

func (nativeWorkflowToolModel) Complete(_ context.Context, messages []agent.Message, specs []agent.ToolSpec) (session.AssistantMessage, error) {
	root := false
	for _, message := range messages {
		if message.Role == "tool" && message.ToolName == "workflow" {
			if !strings.Contains(message.Content, `"child-result"`) && !strings.Contains(message.Content, "child-result") {
				return session.AssistantMessage{}, errors.New("workflow result did not reach parent model")
			}
			return session.AssistantMessage{Content: "workflow complete"}, nil
		}
		if message.Role == "user" && strings.Contains(message.Content, "Run a workflow") {
			root = true
		}
	}
	if !root {
		return session.AssistantMessage{Content: "child-result"}, nil
	}
	found := false
	for _, spec := range specs {
		found = found || spec.Name == "workflow"
	}
	if !found {
		return session.AssistantMessage{}, errors.New("workflow tool was not registered")
	}
	return session.AssistantMessage{ToolCalls: []session.ToolRequest{{
		ID: "workflow-call", Name: "workflow",
		Arguments: `{"script":"return await agent('inspect');","meta":{"name":"inspect-project","description":"Inspect the project"}}`,
	}}}, nil
}

func TestNativeHostModelRunsWorkflowToolThroughChildSession(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeWorkflowToolModel{})
	factory.Storage = rootStore
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.SubmitText(ctx, "Run a workflow"); err != nil {
		t.Fatal(err)
	}
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(rootStore, "turn/end") })
	events := nativeTreeEvents(rootStore)
	for _, kind := range []string{"tool-workflow/run-start", "tool-workflow/agent-start", "tool-workflow/agent-end", "tool-workflow/run-end"} {
		if !nativeTreeHasEvent(rootStore, kind) {
			t.Fatalf("parent v4 Session omitted %s", kind)
		}
	}
	var final bool
	for _, event := range events {
		if event.Type == "assistant/message" && strings.Contains(string(event.Data), "workflow complete") {
			final = true
		}
	}
	if !final {
		t.Fatal("parent Agent did not finish after receiving workflow result")
	}
	entries, err := sessionformat.FoldSubagentCatalog(events, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("workflow child catalog = %+v, %v", entries, err)
	}
	if !nativeTreeHasEvent(nativeTreeNode{tree: tree, id: entries[0].ChildID}, "turn/end") {
		t.Fatal("workflow child did not complete its own v4 turn")
	}
	journal := nativeTreeJSONL(t, rootStore)
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(journal)); err != nil {
		t.Fatalf("workflow parent v4 log: %v", err)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_WORKFLOW_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, journal, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeWorkflowChildSharesHostAndReturnsWithoutParentNotice(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeRuntimeModel{})
	factory.Storage = rootStore
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	result, err := worker.RunWorkflowChild(ctx, "root", "Inspect the project", workflow.AgentOptions{Label: "audit"})
	if err != nil || !result.Completed || result.Value != "done" {
		t.Fatalf("workflow child result = %+v, %v", result, err)
	}
	entries, err := sessionformat.FoldSubagentCatalog(nativeTreeEvents(rootStore), 0)
	if err != nil || len(entries) != 1 || entries[0].Mode != "continuable" || *entries[0].Label != "audit" {
		t.Fatalf("workflow child catalog = %+v, %v", entries, err)
	}
	childStore := nativeTreeNode{tree: tree, id: entries[0].ChildID}
	if !nativeTreeHasEvent(childStore, "subagent/descriptor") || !nativeTreeHasEvent(childStore, "turn/end") {
		t.Fatal("workflow child did not retain its own v4 Session")
	}
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(nativeTreeJSONL(t, childStore))); err != nil {
		t.Fatalf("workflow child v4 log: %v", err)
	}
	for _, event := range nativeTreeEvents(rootStore) {
		if event.Type == "agent/inbox/spliced" {
			t.Fatal("caller-delivered workflow child sent a separate parent notice")
		}
	}
	children, err := worker.ListChildren(ctx, "root")
	if err != nil || len(children) != 1 || children[0].Activity != "inactive" {
		t.Fatalf("completed workflow child status = %+v, %v", children, err)
	}
}

func TestNativeWorkflowChildCancellationCleansUp(t *testing.T) {
	tree := &nativeTreeStore{}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, &nativeGateModel{started: make(chan struct{}), release: make(chan struct{})})
	factory.Storage = nativeTreeNode{tree: tree, id: "root"}
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}}
	if err := worker.Start(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := worker.RunWorkflowChild(ctx, "root", "Wait", workflow.AgentOptions{})
		done <- err
	}()
	model := factory.Model.(*nativeGateModel)
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("workflow child never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("workflow child cancellation = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workflow child did not stop")
	}
	activity, err := worker.Activity()
	if err != nil || !activity.Idle {
		t.Fatalf("cancelled workflow child still active: %+v, %v", activity, err)
	}
}

type nativeTreeStore struct {
	mu    sync.Mutex
	nodes map[string]*nativeRuntimeStore
}

type nativeTreeNode struct {
	tree *nativeTreeStore
	id   string
}

func (n nativeTreeNode) store() *nativeRuntimeStore {
	n.tree.mu.Lock()
	defer n.tree.mu.Unlock()
	if n.tree.nodes == nil {
		n.tree.nodes = make(map[string]*nativeRuntimeStore)
	}
	store := n.tree.nodes[n.id]
	if store == nil {
		store = &nativeRuntimeStore{}
		n.tree.nodes[n.id] = store
	}
	return store
}

func (n nativeTreeNode) CreateNative(ctx context.Context, header sessionformat.Header) (sessionformat.EventJournal, error) {
	return n.store().CreateNative(ctx, header)
}

func (n nativeTreeNode) OpenNative(ctx context.Context) (sessionformat.EventJournal, sessionformat.PhysicalArtifact, error) {
	return n.store().OpenNative(ctx)
}

func (n nativeTreeNode) ChildNativeStorage(id string) (assembly.NativeStorage, error) {
	if id == "" || id == n.id {
		return nil, errors.New("invalid tree child identity")
	}
	return nativeTreeNode{tree: n.tree, id: id}, nil
}

func nativeTreeEvents(n nativeTreeNode) []sessionformat.Event {
	store := n.store()
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]sessionformat.Event(nil), store.events...)
}

func nativeTreeHasEvent(n nativeTreeNode, kind string) bool {
	for _, event := range nativeTreeEvents(n) {
		if event.Type == kind {
			return true
		}
	}
	return false
}

func nativeTreeJSONL(t *testing.T, n nativeTreeNode) []byte {
	t.Helper()
	return nativeRuntimeJournalJSONL(t, n.store())
}

func nativeWaitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if check() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("native child state did not settle")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type nativeTreeJobBuilder struct {
	nativeRuntimeBuilder
	mu         sync.Mutex
	registries map[string]*jobs.Registry
}

type nativeChildJobGateModel struct {
	started    chan struct{}
	release    chan struct{}
	mu         sync.Mutex
	childCalls int
	jobSeen    bool
}

func (m *nativeChildJobGateModel) Complete(ctx context.Context, messages []agent.Message, _ []agent.ToolSpec) (session.AssistantMessage, error) {
	child, jobNotice := false, false
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		child = child || bytes.Contains([]byte(message.Content), []byte("Check with job"))
		jobNotice = jobNotice || bytes.Contains([]byte(message.Content), []byte("background job "))
	}
	if !child {
		return session.AssistantMessage{Content: "root done"}, nil
	}
	m.mu.Lock()
	m.childCalls++
	call := m.childCalls
	m.jobSeen = m.jobSeen || jobNotice
	m.mu.Unlock()
	if call == 1 {
		close(m.started)
		select {
		case <-m.release:
		case <-ctx.Done():
			return session.AssistantMessage{}, ctx.Err()
		}
	}
	return session.AssistantMessage{Content: "child done"}, nil
}

func TestNativeChildJobInjectsIntoActiveChildTurn(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	model := &nativeChildJobGateModel{started: make(chan struct{}), release: make(chan struct{})}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, model)
	builder := &nativeTreeJobBuilder{nativeRuntimeBuilder: factory.Builder.(nativeRuntimeBuilder)}
	factory.Storage, factory.Builder = rootStore, builder
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	childID, err := worker.SpawnChild(ctx, "root", "Research", "Check with job", false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("child model call did not start")
	}
	registry := builder.Registry(childID)
	job, err := registry.Start("shell", "quick child job", func(context.Context, func([]byte)) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MarkVisible(job.ID); err != nil {
		t.Fatal(err)
	}
	worker.children.mu.Lock()
	child := worker.children.children[childID]
	worker.children.mu.Unlock()
	if child == nil {
		t.Fatal("child process disappeared")
	}
	nativeWaitFor(t, func() bool { return len(child.live.Session.Inbox().NextStep) == 1 })
	close(model.release)
	childStore := nativeTreeNode{tree: tree, id: childID}
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(childStore, "turn/end") })
	model.mu.Lock()
	calls, seen := model.childCalls, model.jobSeen
	model.mu.Unlock()
	if calls != 2 || !seen {
		t.Fatalf("child turn calls=%d Job seen=%t", calls, seen)
	}
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(nativeTreeJSONL(t, childStore))); err != nil {
		t.Fatalf("busy child Job v4 journal is invalid: %v", err)
	}
}

func (b *nativeTreeJobBuilder) BuildNative(ctx context.Context, identity assembly.NativeRuntimeIdentity, reason string) (assembly.Resources, error) {
	resources, err := b.nativeRuntimeBuilder.BuildNative(ctx, identity, reason)
	if err != nil {
		return resources, err
	}
	registry, err := jobs.New(context.Background(), 2, 1024, nil)
	if err != nil {
		_ = resources.Close()
		return assembly.Resources{}, err
	}
	resources.Jobs = registry
	closeSkills := resources.Close
	resources.Close = func() error {
		return errors.Join(registry.Close(context.Background()), closeSkills())
	}
	b.mu.Lock()
	if b.registries == nil {
		b.registries = make(map[string]*jobs.Registry)
	}
	b.registries[identity.SessionID] = registry
	b.mu.Unlock()
	return resources, nil
}

func (b *nativeTreeJobBuilder) Registry(id string) *jobs.Registry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.registries[id]
}

func TestNativeChildTurnKeepsRootWorkerActive(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	store := nativeTreeNode{tree: tree, id: "root"}
	model := &nativeGateModel{started: make(chan struct{}), release: make(chan struct{})}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, model)
	factory.Storage = store
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.SpawnChild(ctx, "root", "Research", "Work for a while", false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("child turn did not start")
	}
	activity, err := worker.Activity()
	if err != nil || activity.Idle {
		t.Fatalf("running child was idle: %+v, %v", activity, err)
	}
	close(model.release)
}

func TestNativeChildJobCompletionWakesChildAndKeepsContainerActive(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	model := &nativeJobModel{notified: make(chan struct{})}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, model)
	base := factory.Builder.(nativeRuntimeBuilder)
	builder := &nativeTreeJobBuilder{nativeRuntimeBuilder: base}
	factory.Storage, factory.Builder = rootStore, builder
	worker := &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	childID, err := worker.SpawnChild(ctx, "root", "Research", "Check the facts", false)
	if err != nil {
		t.Fatal(err)
	}
	childStore := nativeTreeNode{tree: tree, id: childID}
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(childStore, "turn/end") })
	registry := builder.Registry(childID)
	if registry == nil {
		t.Fatal("child has no Job Registry")
	}
	release := make(chan struct{})
	job, err := registry.Start("shell", "child task", func(context.Context, func([]byte)) (int, error) {
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
		t.Fatalf("running child Job was idle: %+v, %v", activity, err)
	}
	close(release)
	nativeWaitFor(t, func() bool {
		for _, event := range nativeTreeEvents(childStore) {
			if event.Type == "user/message" && bytes.Contains(event.Data, []byte(`"kind":"tool-jobs"`)) {
				return true
			}
		}
		return false
	})
	select {
	case <-model.notified:
	case <-time.After(5 * time.Second):
		t.Fatal("child Job notice did not reach model")
	}
	nativeWaitFor(t, func() bool {
		completed := 0
		for _, event := range nativeTreeEvents(childStore) {
			if event.Type == "turn/end" {
				completed++
			}
		}
		return completed >= 2
	})
	journal := nativeTreeJSONL(t, childStore)
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(journal)); err != nil {
		t.Fatalf("child Job v4 journal is invalid: %v", err)
	}
	nativeWaitFor(t, func() bool {
		activity, err := worker.Activity()
		return err == nil && activity.Idle
	})
	if exportPath := os.Getenv("SERVERAGENTS_V4_CHILD_JOB_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, journal, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeRuntimeChildHasOwnJournalAndColdResumesInSameWorker(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}
	factory := assembly.NativeFactory{
		Storage: rootStore, Builder: nativeRuntimeBuilder{workspace: t.TempDir()},
		Model: nativeRuntimeModel{}, Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help the user.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2, MaxTurnDuration: 5 * time.Second},
	}
	worker := &NativeRuntime{Factory: factory, Identity: identity}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	childID, err := worker.SpawnChildAt(ctx, "root", "Research", "Find the answer", "research/notes", false)
	if err != nil {
		t.Fatal(err)
	}
	childStore := nativeTreeNode{tree: tree, id: childID}
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(childStore, "subagent/descriptor") })
	if !nativeTreeHasEvent(rootStore, "subagent/catalog") {
		t.Fatal("parent has no durable child catalog")
	}
	childHeader := childStore.store().header
	if childHeader.ParentSession != "root" || childHeader.CWD != "/workspace" || childHeader.Origin != "subagent" {
		t.Fatalf("child header = %+v", childHeader)
	}
	if !nativeTreeHasEvent(childStore, "working-directory/change") {
		t.Fatal("child effective cwd was not persisted separately from its inherited header cwd")
	}
	if err := worker.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	resumed := &NativeRuntime{Factory: factory, Identity: identity}
	if err := resumed.Start(ctx, true); err != nil {
		t.Fatal(err)
	}
	defer resumed.Abort()
	children, err := resumed.ListChildren(ctx, "root")
	if err != nil || len(children) != 1 || children[0].SessionID != childID || children[0].Activity != "inactive" {
		t.Fatalf("resumed child catalog = %+v, %v", children, err)
	}
	if _, err := resumed.SendChildMessage(ctx, "root", childID, "Continue with one more check"); err != nil {
		t.Fatal(err)
	}
	nativeWaitFor(t, func() bool {
		for _, event := range nativeTreeEvents(childStore) {
			if event.Type != "agent/inbox/spliced" {
				continue
			}
			var data map[string]json.RawMessage
			if json.Unmarshal(event.Data, &data) == nil && len(data["inserted"]) > 0 &&
				bytes.Contains(data["inserted"], []byte(`"kind":"agent-message"`)) {
				return true
			}
		}
		return false
	})
	if err := resumed.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRuntimeForkChildCopiesOnlyCompletedTurnPrefix(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	rootStore := nativeTreeNode{tree: tree, id: "root"}
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "root", Workspace: "/workspace"}
	worker := &NativeRuntime{Factory: assembly.NativeFactory{
		Storage: rootStore, Builder: nativeRuntimeBuilder{workspace: t.TempDir()},
		Model: nativeRuntimeModel{}, Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help the user.",
		Limits: agent.Limits{MaxSteps: 2, MaxToolCalls: 2, MaxTurnDuration: 5 * time.Second},
	}, Identity: identity}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Abort()
	if _, err := worker.SubmitText(ctx, "first completed request"); err != nil {
		t.Fatal(err)
	}
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(rootStore, "turn/end") })
	parentEvents := nativeTreeEvents(rootStore)
	cut := -1
	for index, event := range parentEvents {
		if event.Type == "turn/end" {
			cut = index
		}
	}
	if cut < 0 {
		t.Fatal("parent has no closed turn")
	}
	childID, err := worker.SpawnChild(ctx, "root", "Forked research", "Continue from parent context", true)
	if err != nil {
		t.Fatal(err)
	}
	childStore := nativeTreeNode{tree: tree, id: childID}
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(childStore, "subagent/descriptor") })
	childEvents := nativeTreeEvents(childStore)
	if !childStore.store().header.IsSeeded || len(childEvents) <= cut+5 {
		t.Fatalf("fork did not retain a seeded prefix: %d events", len(childEvents))
	}
	for index := 0; index <= cut; index++ {
		if childEvents[index].Type != parentEvents[index].Type || string(childEvents[index].Data) != string(parentEvents[index].Data) {
			t.Fatalf("fork prefix differs at event %d", index)
		}
	}
	if childEvents[cut+1].Type != "session/end-seed" || string(childEvents[cut+1].Data) != `{"inherited":true}` {
		t.Fatalf("fork inherited marker = %+v", childEvents[cut+1])
	}
	for index, kind := range []string{"sandbox/mode", "approval/policy", "permission/preset"} {
		if childEvents[cut+2+index].Type != kind {
			t.Fatalf("delegated child policy event %d = %s", index, childEvents[cut+2+index].Type)
		}
	}
	if childEvents[cut+5].Type != "agent/inbox/spliced" {
		t.Fatalf("first child inbox event = %s", childEvents[cut+5].Type)
	}
	if err := worker.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	resumed := &NativeRuntime{Factory: worker.Factory, Identity: identity}
	if err := resumed.Start(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.SendChildMessage(ctx, "root", childID, "Check the inherited context once more"); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_FORK_PARENT_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, nativeTreeJSONL(t, rootStore), 0o600); err != nil {
			t.Fatalf("export fork parent v4 journal: %v", err)
		}
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_FORK_CHILD_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, nativeTreeJSONL(t, childStore), 0o600); err != nil {
			t.Fatalf("export fork child v4 journal: %v", err)
		}
	}
}
