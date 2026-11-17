package tools

import (
	"context"
	"encoding/json"
	"testing"

	"go-bot/harness/agent"
	"go-bot/harness/jobs"
)

func TestUpstreamToolParameterRootsRemainOpen(t *testing.T) {
	specs := []agent.ToolSpec{
		(NativeGetGoal{}).Spec(), (NativeCreateGoal{}).Spec(), (NativeUpdateGoal{}).Spec(),
		(Subagent{}).Spec(), (Subagent{Fork: true}).Spec(),
		(SendMessage{}).Spec(), (InterruptAgent{}).Spec(), (ListAgents{}).Spec(),
		(NativeSchedule{Name: "schedule_create"}).Spec(), (NativeSchedule{Name: "schedule_list"}).Spec(),
		(NativeSchedule{Name: "schedule_view"}).Spec(), (NativeSchedule{Name: "schedule_edit"}).Spec(),
		(NativeTodoWrite{}).Spec(), exitPlanModeSpec(), (ReadImage{}).Spec(),
		(RipgrepSearch{Name: "glob"}).Spec(), (RipgrepSearch{Name: "grep"}).Spec(),
		(NativeWorkflow{}).Spec(), (JobOutput{}).Spec(), (JobList{}).Spec(), (JobKill{}).Spec(),
	}
	for _, spec := range specs {
		var parameters map[string]any
		if err := json.Unmarshal(spec.Parameters, &parameters); err != nil {
			t.Fatalf("%s schema: %v", spec.Name, err)
		}
		if parameters["type"] != "object" || parameters["additionalProperties"] == false {
			t.Errorf("%s parameter root is not open: %s", spec.Name, spec.Parameters)
		}
	}
}

func TestOpenRootDoesNotOpenNestedToolObjects(t *testing.T) {
	var args struct {
		Objective string `json:"objective"`
	}
	if err := decodeGoalArgs(json.RawMessage(`{"objective":"goal","extension":{"any":true}}`), &args); err != nil || args.Objective != "goal" {
		t.Fatalf("open Goal root = %+v, %v", args, err)
	}
	if _, err := (NativeSchedule{Name: "schedule_view"}).decode(json.RawMessage(`{"id":"task","extra":null}`)); err != nil {
		t.Fatalf("open Schedule root rejected extra null: %v", err)
	}
	if _, err := decodeNativeTodoArgs(json.RawMessage(`{"todos":[{"content":"step","status":"pending"}],"extra":true}`), true); err != nil {
		t.Fatalf("open Todo root rejected extra field: %v", err)
	}
	if _, err := decodeNativeTodoArgs(json.RawMessage(`{"todos":[{"content":"step","status":"pending","extra":true}]}`), true); err == nil {
		t.Fatal("Todo accepted undeclared nested item field")
	}
}

func TestToolArgumentDecodersRejectTrailingValues(t *testing.T) {
	stub := &shellWorkdirStub{}
	shell := Shell{Executor: stub, DefaultWorkdir: "/root/harness"}
	invalidShell := json.RawMessage(`{"command":"touch unexpected"}{"command":"true"}`)
	if err := shell.Validate(invalidShell); err == nil {
		t.Fatal("shellexecute accepted two argument objects")
	}
	if _, err := shell.Execute(context.Background(), invalidShell); err == nil || stub.cwd != "" {
		t.Fatalf("invalid shell arguments executed: cwd=%q, error=%v", stub.cwd, err)
	}
	if _, err := decodeGenerateImageArgs(json.RawMessage(`{"prompt":"draw"} true`)); err == nil {
		t.Fatal("generateimage accepted a trailing JSON value")
	}
	registry := &jobs.Registry{}
	if err := (JobOutput{Registry: registry}).Validate(json.RawMessage(`{"job_id":"job-1"}{"wait":true}`)); err == nil {
		t.Fatal("job_output accepted two argument objects")
	}
	if err := (JobKill{Registry: registry}).Validate(json.RawMessage(`{"job_id":"job-1"}{"reason":"cancel"}`)); err == nil {
		t.Fatal("job_kill accepted two argument objects")
	}
	var args struct {
		Name string `json:"name"`
	}
	if err := decodeOneToolObject(json.RawMessage(`null`), &args); err == nil {
		t.Fatal("typed tool arguments accepted null rather than an object")
	}
}
