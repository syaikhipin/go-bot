package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"go-bot/harness/jobs"
	"go-bot/harness/session"
	"go-bot/harness/workflow"
)

type workflowJournal struct {
	mu     sync.Mutex
	events []sessionformat.Event
}

func (j *workflowJournal) Append(_ context.Context, events []sessionformat.Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, events...)
	return nil
}

func (*workflowJournal) Close() error { return nil }

func (j *workflowJournal) types() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	kinds := make([]string, len(j.events))
	for index, event := range j.events {
		kinds[index] = event.Type
	}
	return kinds
}

type workflowCoordinatorFunc func(context.Context, string, string, workflow.AgentOptions, func(string)) (workflow.AgentResult, error)

func (f workflowCoordinatorFunc) RunWorkflowChildObserved(ctx context.Context, parentID, prompt string, options workflow.AgentOptions, started func(string)) (workflow.AgentResult, error) {
	return f(ctx, parentID, prompt, options, started)
}

func testWorkflowSession(t *testing.T) (*sessionformat.LiveSession, *workflowJournal) {
	t.Helper()
	journal := &workflowJournal{}
	session, err := sessionformat.NewLiveSession(sessionformat.Header{
		Type: "session", Version: sessionformat.Version, ID: "root", CreatedAt: 1, CWD: "/root/harness/workspaces/root",
	}, journal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, journal
}

func TestNativeWorkflowForegroundRecordsParentRunAndMembers(t *testing.T) {
	session, journal := testWorkflowSession(t)
	tool := NativeWorkflow{Session: session, ParentID: "root", Coordinator: workflowCoordinatorFunc(
		func(_ context.Context, parentID, prompt string, options workflow.AgentOptions, started func(string)) (workflow.AgentResult, error) {
			if parentID != "root" || options.Phase != "review" {
				t.Errorf("child received parent=%q options=%+v", parentID, options)
			}
			started("child-" + prompt)
			return workflow.AgentResult{Value: prompt + " done", Completed: true}, nil
		})}
	input := json.RawMessage(`{"script":"phase('review'); const rows = await parallel([() => agent('a'), () => agent('b')]); return {name:meta.name,rows};","meta":{"name":"review-files","description":"Review files"}}`)
	if err := tool.Validate(input); err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), input)
	if err != nil || !strings.Contains(output, `"rows": [`) || !strings.Contains(output, `"review-files"`) {
		t.Fatalf("workflow result = %q, %v", output, err)
	}
	kinds := journal.types()
	if len(kinds) != 6 || kinds[0] != "tool-workflow/run-start" || kinds[5] != "tool-workflow/run-end" {
		t.Fatalf("workflow event sequence = %v", kinds)
	}
	starts, ends := 0, 0
	for _, kind := range kinds {
		if kind == "tool-workflow/agent-start" {
			starts++
		}
		if kind == "tool-workflow/agent-end" {
			ends++
		}
	}
	if starts != 2 || ends != 2 {
		t.Fatalf("workflow member pairs = %d starts / %d ends", starts, ends)
	}
}

func TestNativeWorkflowPreservesOpenMetaForScript(t *testing.T) {
	session, _ := testWorkflowSession(t)
	tool := NativeWorkflow{Session: session, ParentID: "root", Coordinator: workflowCoordinatorFunc(
		func(context.Context, string, string, workflow.AgentOptions, func(string)) (workflow.AgentResult, error) {
			return workflow.AgentResult{}, nil
		})}
	input := json.RawMessage(`{"script":"return {owner:meta.owner, phaseColor:meta.phases[0].color};","meta":{"name":"open-meta","description":"Check metadata","owner":"alice","phases":[{"title":"review","color":"blue"}]}}`)
	output, err := tool.Execute(context.Background(), input)
	if err != nil || !strings.Contains(output, `"owner": "alice"`) || !strings.Contains(output, `"phaseColor": "blue"`) {
		t.Fatalf("open workflow meta was not exposed to script: %q, %v", output, err)
	}
	if err := tool.Validate(json.RawMessage(`{"script":"return 1;","meta":{"name":"x","description":"x"},"unknown":true}`)); err != nil {
		t.Fatalf("upstream-open top-level Workflow argument was rejected: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Spec().Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] == false {
		t.Fatal("Workflow parameter root must remain open like defineTool's implicit root")
	}
}

func TestNativeWorkflowBackgroundIsManagedJob(t *testing.T) {
	session, journal := testWorkflowSession(t)
	registry, err := jobs.New(context.Background(), 2, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close(context.Background())
	startedChild := make(chan struct{})
	release := make(chan struct{})
	tool := NativeWorkflow{Session: session, ParentID: "root", Jobs: registry, Coordinator: workflowCoordinatorFunc(
		func(ctx context.Context, _, _ string, _ workflow.AgentOptions, started func(string)) (workflow.AgentResult, error) {
			if kinds := journal.types(); len(kinds) == 0 || kinds[0] != "tool-workflow/run-start" {
				t.Errorf("background child started before durable run-start: %v", kinds)
			}
			started("child-1")
			close(startedChild)
			select {
			case <-release:
				return workflow.AgentResult{Value: "done", Completed: true}, nil
			case <-ctx.Done():
				return workflow.AgentResult{}, ctx.Err()
			}
		})}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"script":"return await agent('task');","meta":{"name":"background-run","description":"Run a child"},"run_in_background":true}`))
	if err != nil || !strings.Contains(output, "job ") {
		t.Fatalf("background acceptance = %q, %v", output, err)
	}
	select {
	case <-startedChild:
	case <-time.After(3 * time.Second):
		t.Fatal("background workflow did not start")
	}
	close(release)
	for _, snapshot := range registry.List() {
		if snapshot.Kind != "workflow" {
			continue
		}
		status, err := registry.Wait(context.Background(), snapshot.ID, 3*time.Second)
		if err != nil || status.Status != jobs.Completed {
			t.Fatalf("background workflow status = %+v, %v", status, err)
		}
		result, err := registry.Consume(snapshot.ID)
		if err != nil || !strings.Contains(result.Text, "Return value:") {
			t.Fatalf("background workflow output = %+v, %v", result, err)
		}
	}
	if kinds := journal.types(); len(kinds) != 4 || kinds[0] != "tool-workflow/run-start" || kinds[3] != "tool-workflow/run-end" {
		t.Fatalf("background workflow events = %v", kinds)
	}
}
