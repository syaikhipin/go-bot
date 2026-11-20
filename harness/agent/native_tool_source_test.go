package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go-bot/harness/session"
	"go-bot/internal/modeldata"
)

type dynamicProbe struct {
	label string
	calls int
}

func (p *dynamicProbe) Spec() ToolSpec {
	return ToolSpec{Name: "extension_probe", Description: p.label, Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`)}
}
func (*dynamicProbe) Validate(json.RawMessage) error { return nil }
func (p *dynamicProbe) Execute(context.Context, json.RawMessage) (string, error) {
	p.calls++
	return p.label, nil
}

type dynamicModel func(context.Context, []Message, []ToolSpec) (modeldata.AssistantMessage, error)

func (f dynamicModel) Complete(ctx context.Context, messages []Message, specs []ToolSpec) (modeldata.AssistantMessage, error) {
	return f(ctx, messages, specs)
}

func TestStepPinsDynamicHandlerAndRefreshesNextStep(t *testing.T) {
	ctx := context.Background()
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "dynamic-steps", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	old, next := &dynamicProbe{label: "old"}, &dynamicProbe{label: "next"}
	current := old
	steps := 0
	model := dynamicModel(func(_ context.Context, _ []Message, specs []ToolSpec) (modeldata.AssistantMessage, error) {
		steps++
		want := "old"
		if steps > 1 {
			want = "next"
		}
		if len(specs) != 1 || specs[0].Description != want {
			t.Fatalf("step %d has wrong definition: %+v", steps, specs)
		}
		if steps == 1 {
			// A config change during a model request must not retarget its call.
			current = next
			return modeldata.AssistantMessage{ToolCalls: []modeldata.ToolRequest{{ID: "probe-call", Name: "extension_probe", Arguments: `{}`}}}, nil
		}
		return modeldata.AssistantMessage{Content: "done"}, nil
	})
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", nil, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	sources := 0
	loop.SetToolSource(func(_ context.Context, id string) ([]Tool, error) {
		if id != "dynamic-steps" {
			t.Fatalf("wrong Session scope: %q", id)
		}
		sources++
		return []Tool{current}, nil
	})
	if _, err := loop.AcceptText(ctx, "probe"); err != nil {
		t.Fatal(err)
	}
	if ran, answer, err := loop.RunNext(ctx); !ran || err != nil || answer != "done" {
		t.Fatalf("dynamic turn: ran=%v answer=%q err=%v", ran, answer, err)
	}
	if old.calls != 1 || next.calls != 0 || sources != 2 {
		t.Fatalf("handler changed within step: old=%d next=%d resolutions=%d", old.calls, next.calls, sources)
	}
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
}

func TestBadDynamicSetDoesNotShadowBuiltInOrPartiallyPublish(t *testing.T) {
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "dynamic-failure", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	builtin := &dynamicProbe{label: "built-in"}
	loop, err := NewNativeLoop(live, &nativeAnswerModel{}, "provider", "model", "prompt", []Tool{builtin}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	loop.SetToolSource(func(context.Context, string) ([]Tool, error) {
		return []Tool{&dynamicProbe{label: "shadow"}}, nil
	})
	if err := loop.refreshTools(context.Background()); err == nil || loop.tools["extension_probe"] != builtin {
		t.Fatalf("shadow registered: %v", err)
	}
	loop.SetToolSource(func(context.Context, string) ([]Tool, error) { return nil, errors.New("extension unavailable") })
	if err := loop.refreshTools(context.Background()); err == nil || len(loop.toolSpecs) != 1 {
		t.Fatalf("source failure changed current set: %v", err)
	}
}
