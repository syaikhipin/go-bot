package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go-bot/harness/agent"
	sessionformat "go-bot/harness/session"
	"go-bot/internal/modeldata"
)

type structuredResultModel struct{ calls int }

func (m *structuredResultModel) Complete(context.Context, []agent.Message, []agent.ToolSpec) (modeldata.AssistantMessage, error) {
	m.calls++
	if m.calls == 1 {
		return modeldata.AssistantMessage{ToolCalls: []modeldata.ToolRequest{{ID: "answer", Name: "structured_output", Arguments: `{"answer":42}`}}}, nil
	}
	return modeldata.AssistantMessage{Content: "done"}, nil
}

type structuredResultJournal struct {
	nativeTodoJournal
	tool      *NativeStructuredOutput
	fail      bool
	sawResult bool
	t         *testing.T
}

func (j *structuredResultJournal) Append(ctx context.Context, events []sessionformat.Event) error {
	for _, event := range events {
		if event.Type == "tool/result" {
			j.sawResult = true
			if _, ok := j.tool.Captured(); ok {
				j.t.Error("structured output confirmed before durable result commit")
			}
			if j.fail {
				return errors.New("result journal unavailable")
			}
		}
	}
	return j.nativeTodoJournal.Append(ctx, events)
}
func TestStructuredOutputConfirmsOnlyFinalDurablyCommittedSuccess(t *testing.T) {
	for _, mode := range []string{"success", "post-block", "commit-failure"} {
		t.Run(mode, func(t *testing.T) {
			tool, err := NewNativeStructuredOutput(map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer"}}, "required": []string{"answer"}})
			if err != nil {
				t.Fatal(err)
			}
			journal := &structuredResultJournal{tool: tool, t: t, fail: mode == "commit-failure"}
			live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "structured-" + mode, CreatedAt: 1, CWD: t.TempDir()}, journal)
			if err != nil {
				t.Fatal(err)
			}
			loop, err := agent.NewNativeLoop(live, &structuredResultModel{}, "p", "m", "prompt", []agent.Tool{tool}, 3, 3)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "post-block" {
				loop.SetToolInvocationSource(func(context.Context, string) (agent.ToolInvocationFactory, error) {
					return func(ctx context.Context, execution agent.ToolExecution, dispatch func(context.Context) modeldata.ToolResult, approve func(context.Context, agent.ToolPolicyAsk) error) (*agent.ToolInvocation, error) {
						return agent.NewToolInvocation(ctx, execution, agent.ToolPipelineProcessors{PostExecute: func(context.Context, agent.ToolPipelineResult) (agent.PostToolDecision, error) {
							return agent.PostToolDecision{Kind: "block", Feedback: []json.RawMessage{json.RawMessage(`{"type":"text","text":"result rejected"}`)}}, nil
						}}, func(ctx context.Context) (agent.ToolPipelineResult, error) {
							return agent.EncodeToolPipelineResult(dispatch(ctx))
						}, approve), nil
					}, nil
				})
			}
			_, err = loop.RunTextTurn(t.Context(), "Return the structured answer")
			if (err != nil) != (mode == "commit-failure") {
				t.Fatalf("run error=%v", err)
			}
			value, captured := tool.Captured()
			if !journal.sawResult || captured != (mode == "success") {
				t.Fatalf("result attempted=%v captured=%v value=%v", journal.sawResult, captured, value)
			}
			if captured && value.(map[string]any)["answer"] != float64(42) {
				t.Fatalf("value=%v", value)
			}
			if mode != "success" && tool.pending != nil {
				t.Fatal("provisional state survived rejection or commit failure")
			}
		})
	}
}
