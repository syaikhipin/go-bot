package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go-bot/harness/agent"
)

type selectableChildCoordinator struct {
	testChildCoordinator
	selected agent.ModelRoute
}

func (c *selectableChildCoordinator) SpawnChildSelected(_ context.Context, _ string, label, prompt, cwd string, route agent.ModelRoute) (string, error) {
	c.label, c.prompt, c.cwd, c.selected = label, prompt, cwd, route
	return "selected-child", nil
}

type testChildCoordinator struct {
	label, prompt, cwd, sender, target, content string
	fork, interrupted                           bool
}

func (c *testChildCoordinator) SpawnChildAt(_ context.Context, _ string, label, prompt, cwd string, fork bool) (string, error) {
	c.label, c.prompt, c.cwd, c.fork = label, prompt, cwd, fork
	return "child-1", nil
}

func (c *testChildCoordinator) SpawnChild(_ context.Context, _ string, label, prompt string, fork bool) (string, error) {
	c.label, c.prompt, c.fork = label, prompt, fork
	return "child-1", nil
}

func (c *testChildCoordinator) SendChildMessage(_ context.Context, sender, target, content string) (string, error) {
	c.sender, c.target, c.content = sender, target, content
	return "message-1", nil
}

func (c *testChildCoordinator) InterruptChild(_ context.Context, _, target string) (string, error) {
	c.target, c.interrupted = target, true
	return "stopping", nil
}

func (*testChildCoordinator) ListChildren(context.Context, string) ([]ChildStatus, error) {
	return []ChildStatus{{SessionID: "child-1", Label: "Research", Activity: "idle"}}, nil
}

func TestSubagentToolsUseCurrentModelFacingNamesAndFields(t *testing.T) {
	ctx := context.Background()
	coordinator := &testChildCoordinator{}
	for _, fork := range []bool{false, true} {
		tool := Subagent{Coordinator: coordinator, ParentID: "parent", Fork: fork}
		if strings.Contains(string(tool.Spec().Parameters), `"label"`) || !strings.Contains(string(tool.Spec().Parameters), `"description"`) {
			t.Fatalf("delegation schema is stale: %s", tool.Spec().Parameters)
		}
		result, err := tool.Execute(ctx, json.RawMessage(`{"description":"Research","prompt":"Find evidence"}`))
		if err != nil || result != `{"kind":"activation","subagentId":"child-1"}` || coordinator.label != "Research" || coordinator.prompt != "Find evidence" || coordinator.fork != fork {
			t.Fatalf("delegation = %s, %v, %+v", result, err, coordinator)
		}
		if err := tool.Validate(json.RawMessage(`{"label":"old","prompt":"Find evidence"}`)); err == nil {
			t.Fatal("old label field was admitted")
		}
		result, err = tool.Execute(ctx, json.RawMessage(`{"cwd":"research","description":"Research","prompt":"Find evidence"}`))
		if err != nil || coordinator.cwd != "research" {
			t.Fatalf("delegation cwd = %s, %v, %+v", result, err, coordinator)
		}
	}
	send := SendMessage{Coordinator: coordinator, SenderID: "parent"}
	if strings.Contains(string(send.Spec().Parameters), `"session_id"`) || !strings.Contains(string(send.Spec().Parameters), `"agent_id"`) {
		t.Fatalf("send_message schema is stale: %s", send.Spec().Parameters)
	}
	result, err := send.Execute(ctx, json.RawMessage(`{"agent_id":"child-1","message":"Continue"}`))
	if err != nil || result != `{"messageId":"message-1"}` || coordinator.target != "child-1" || coordinator.content != "Continue" {
		t.Fatalf("send_message = %s, %v, %+v", result, err, coordinator)
	}
	interrupt := InterruptAgent{Coordinator: coordinator, ParentID: "parent"}
	result, err = interrupt.Execute(ctx, json.RawMessage(`{"agent_id":"child-1"}`))
	if err != nil || result != `{"accepted":true}` || !coordinator.interrupted {
		t.Fatalf("interrupt_agent = %s, %v", result, err)
	}
	list := ListAgents{Coordinator: coordinator, ParentID: "parent"}
	result, err = list.Execute(ctx, json.RawMessage(`{"scope":"children"}`))
	if err != nil || result != `[{"id":"child-1","kind":"child","label":"Research","status":"inactive"}]` {
		t.Fatalf("list_agents children = %s, %v", result, err)
	}
	result, err = list.Execute(ctx, json.RawMessage(`{"scope":"descendants"}`))
	if err != nil || !strings.Contains(result, `"parent":"parent"`) || !strings.Contains(result, `"depth":1`) {
		t.Fatalf("list_agents descendants = %s, %v", result, err)
	}
}

func TestSubagentsAlwaysInheritMain(t *testing.T) {
	for _, fork := range []bool{false, true} {
		tool := Subagent{Coordinator: &testChildCoordinator{}, ParentID: "parent", Fork: fork}
		if strings.Contains(string(tool.Spec().Parameters), `"provider"`) {
			t.Fatal("selection exposed")
		}
		if tool.Validate(json.RawMessage(`{"description":"task","prompt":"task","provider":"other","model":"other"}`)) == nil {
			t.Fatal("selection accepted")
		}
	}
}
