package agent

import (
	"context"
	"encoding/json"
	"testing"

	"go-bot/harness/session"
	"go-bot/internal/modeldata"
)

func TestToolPipelineResultAndDeferredContextEnterTheExistingJournalInOrder(t *testing.T) {
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "policy-loop", CWD: "/tmp", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	probe := &dynamicProbe{label: "raw"}
	steps, resolutions := 0, 0
	model := dynamicModel(func(_ context.Context, messages []Message, _ []ToolSpec) (modeldata.AssistantMessage, error) {
		steps++
		if steps == 1 {
			return modeldata.AssistantMessage{ToolCalls: []modeldata.ToolRequest{{ID: "native-policy-call", Name: "extension_probe", Arguments: `{}`}}}, nil
		}
		toolIndex, contextIndex := -1, -1
		for i, message := range messages {
			if message.Role == "tool" && message.Content == "reviewed" {
				toolIndex = i
			}
			if message.Role == "user" && message.Content == "policy context" {
				contextIndex = i
			}
		}
		if toolIndex < 0 || contextIndex <= toolIndex {
			t.Errorf("policy outcome/context missing or unordered: %+v", messages)
		}
		return modeldata.AssistantMessage{Content: "done"}, nil
	})
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", []Tool{probe}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	loop.SetToolInvocationSource(func(_ context.Context, id string) (ToolInvocationFactory, error) {
		resolutions++
		if id != "policy-loop" {
			t.Error("wrong policy Session")
		}
		return func(ctx context.Context, execution ToolExecution, dispatch func(context.Context) modeldata.ToolResult, approve func(context.Context, ToolPolicyAsk) error) (*ToolInvocation, error) {
			turn, step, ok := ToolPositionFromContext(ctx)
			if !ok || turn != execution.Turn || step != execution.Step || execution.CWD != "/tmp" || execution.CallID != "native-policy-call" {
				t.Errorf("wrong execution context: %+v", execution)
			}
			return NewToolInvocation(ctx, execution, ToolPipelineProcessors{PostExecute: func(context.Context, ToolPipelineResult) (PostToolDecision, error) {
				return PostToolDecision{Kind: "accept", Content: pipelineText("reviewed"), AdditionalContexts: []json.RawMessage{json.RawMessage(`{"id":"context","role":"user","source":{"kind":"test-policy","form":"notice"},"content":[{"type":"text","text":"policy context"}]}`)}}, nil
			}}, func(ctx context.Context) (ToolPipelineResult, error) { return EncodeToolPipelineResult(dispatch(ctx)) }, approve), nil
		}, nil
	})
	if _, err = loop.AcceptText(t.Context(), "run probe"); err != nil {
		t.Fatal(err)
	}
	if ran, answer, err := loop.RunNext(t.Context()); !ran || answer != "done" || err != nil {
		t.Fatalf("ran=%v answer=%q err=%v", ran, answer, err)
	}
	if probe.calls != 1 || resolutions != 2 {
		t.Fatalf("calls=%d resolutions=%d", probe.calls, resolutions)
	}
	if err = sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
}

func TestToolPreparationCapturesActualToolBeforeTableChanges(t *testing.T) {
	first, replacement := &dynamicProbe{label: "first"}, &dynamicProbe{label: "replacement"}
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "fixed-tool", CreatedAt: 1, CWD: t.TempDir()}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(live, &nativeAnswerModel{}, "p", "m", "prompt", []Tool{first}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	prepared := loop.prepareNativeTool(t.Context(), 1, 1, modeldata.ToolRequest{ID: "fixed", Name: first.Spec().Name, Arguments: `{}`})
	loop.tools[first.Spec().Name] = replacement
	result := prepared.complete(prepared.dispatch())
	if result.IsError || first.calls != 1 || replacement.calls != 0 {
		t.Fatalf("first=%d replacement=%d result=%+v", first.calls, replacement.calls, result)
	}
}

func TestNativeDispatchMarksOnlyTheBusinessBodyAsStarted(t *testing.T) {
	for _, tc := range []struct {
		name, tool, args string
		started          bool
	}{
		{"invalid-json", "extension_probe", "{", false},
		{"unknown-tool", "missing", "{}", false},
		{"business-body", "extension_probe", "{}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "body-boundary", CreatedAt: 1, CWD: t.TempDir()}, &nativeTestJournal{})
			if err != nil {
				t.Fatal(err)
			}
			probe := &dynamicProbe{label: "body"}
			loop, err := NewNativeLoop(live, &nativeAnswerModel{}, "p", "m", "prompt", []Tool{probe}, 2, 2)
			if err != nil {
				t.Fatal(err)
			}
			prepared := loop.prepareNativeTool(t.Context(), 1, 1, modeldata.ToolRequest{ID: "call", Name: tc.tool, Arguments: tc.args})
			result := prepared.complete(prepared.dispatch())
			if got := prepared.invocation.started.Load(); got != tc.started {
				t.Fatalf("bodyStarted=%v want=%v result=%+v", got, tc.started, result)
			}
			if (probe.calls > 0) != tc.started {
				t.Fatalf("business calls=%d", probe.calls)
			}
		})
	}
}
