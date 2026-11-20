package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func pipelineText(text string) []json.RawMessage {
	block, _ := json.Marshal(map[string]string{"type": "text", "text": text})
	return []json.RawMessage{block}
}
func pipelineSuccess(text string) ToolPipelineResult {
	value, _ := json.Marshal(text)
	return ToolPipelineResult{Value: value, Content: pipelineText(text)}
}
func runToolInvocation(p *ToolInvocation) ToolPipelineResult {
	prepared := p.Prepare()
	outcome := prepared.Outcome
	if prepared.Dispatch {
		outcome = p.Dispatch()
	}
	return p.Complete(outcome)
}

func TestGoToolPipelineOwnsApprovalAndSettlementOrder(t *testing.T) {
	var trace []string
	processors := ToolPipelineProcessors{
		Project: func(context.Context, ToolPipelineResult) ([]json.RawMessage, error) {
			trace = append(trace, "project")
			return pipelineText("projected"), nil
		},
		PostExecute: func(_ context.Context, result ToolPipelineResult) (PostToolDecision, error) {
			trace = append(trace, "post")
			if string(result.Content[0]) != string(pipelineText("projected")[0]) {
				t.Error("post lost projected content")
			}
			return PostToolDecision{Kind: "accept", Content: pipelineText("accepted")}, nil
		},
		Finalize: func(_ context.Context, result ToolPipelineResult) ([]json.RawMessage, error) {
			trace = append(trace, "finalize")
			if string(result.Content[0]) != string(pipelineText("accepted")[0]) {
				t.Error("finalizer lost accepted content")
			}
			return nil, nil
		},
	}
	p := NewToolInvocation(t.Context(), ToolExecution{SessionID: "session"}, processors,
		func(context.Context) (ToolPipelineResult, error) {
			trace = append(trace, "body")
			return pipelineSuccess("raw"), nil
		},
		func(_ context.Context, ask ToolPolicyAsk) error {
			trace = append(trace, "approval")
			if ask.Reason != "confirm" {
				t.Error("reason lost")
			}
			return nil
		})
	p.SetPreExecutePolicy(func(context.Context) (PreToolDecision, error) {
		trace = append(trace, "pre")
		return PreToolDecision{Kind: "ask", Reason: "confirm"}, nil
	})
	result := runToolInvocation(p)
	if want := []string{"pre", "approval", "body", "project", "post", "finalize"}; !reflect.DeepEqual(trace, want) || result.IsError {
		t.Fatalf("trace=%v result=%+v", trace, result)
	}
	if second := p.Dispatch(); !second.Result.IsError {
		t.Fatal("a recorded body was dispatched twice")
	}
}

func TestGoToolPipelineKeepsFailureBranchPostSemantics(t *testing.T) {
	for _, where := range []string{"pre-denial", "pre-exception", "invalid-decision", "body-error", "body-panic", "cancel-before-prepare"} {
		t.Run(where, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			post, finalized, bodies := 0, 0, 0
			p := NewToolInvocation(ctx, ToolExecution{}, ToolPipelineProcessors{
				PostExecute: func(context.Context, ToolPipelineResult) (PostToolDecision, error) {
					post++
					return PostToolDecision{Kind: "accept"}, nil
				},
				Finalize: func(context.Context, ToolPipelineResult) ([]json.RawMessage, error) { finalized++; return nil, nil },
			}, func(context.Context) (ToolPipelineResult, error) {
				bodies++
				if where == "body-panic" {
					panic("body failed")
				}
				return ToolPipelineResult{}, errors.New("body failed")
			}, nil)
			p.SetPreExecutePolicy(func(context.Context) (PreToolDecision, error) {
				switch where {
				case "pre-denial":
					return PreToolDecision{Kind: "deny", Reason: "denied"}, nil
				case "pre-exception":
					return PreToolDecision{}, errors.New("policy failed")
				case "invalid-decision":
					return PreToolDecision{Kind: "unexpected"}, nil
				}
				return PreToolDecision{Kind: "allow"}, nil
			})
			if where == "cancel-before-prepare" {
				cancel()
			}
			result := runToolInvocation(p)
			wantPost, wantBodies := 0, 0
			if where == "pre-denial" || where == "body-error" || where == "body-panic" {
				wantPost = 1
			}
			if where == "body-error" || where == "body-panic" {
				wantBodies = 1
			}
			if !result.IsError || post != wantPost || finalized != 1 || bodies != wantBodies {
				t.Fatalf("result=%+v post=%d final=%d bodies=%d", result, post, finalized, bodies)
			}
		})
	}
}

func TestGoToolPipelineBlockDiscardsToolContextsAndConcludeTurn(t *testing.T) {
	contextJSON := json.RawMessage(`{"id":"policy-context","role":"user","content":[]}`)
	result := runToolInvocation(NewToolInvocation(t.Context(), ToolExecution{}, ToolPipelineProcessors{
		PostExecute: func(context.Context, ToolPipelineResult) (PostToolDecision, error) {
			return PostToolDecision{Kind: "block", Feedback: pipelineText("redacted"), AdditionalContexts: []json.RawMessage{contextJSON}}, nil
		},
	}, func(context.Context) (ToolPipelineResult, error) {
		result := pipelineSuccess("private")
		result.ConcludesTurn = true
		result.AdditionalContexts = []json.RawMessage{json.RawMessage(`{"id":"tool-context"}`)}
		return result, nil
	}, nil))
	if !result.IsError || result.ConcludesTurn || len(result.AdditionalContexts) != 1 || string(result.AdditionalContexts[0]) != string(contextJSON) {
		t.Fatalf("blocked=%+v", result)
	}
}

func TestGoToolPipelineAskWithoutApprovalChannelNeverDispatches(t *testing.T) {
	p := NewToolInvocation(t.Context(), ToolExecution{SessionID: "s"}, ToolPipelineProcessors{}, func(context.Context) (ToolPipelineResult, error) {
		t.Fatal("unavailable approval dispatched")
		return ToolPipelineResult{}, nil
	}, nil)
	p.SetPreExecutePolicy(func(context.Context) (PreToolDecision, error) { return PreToolDecision{Kind: "ask"}, nil })
	result := runToolInvocation(p)
	if !result.IsError || result.Error.Info.Code != "APPROVAL_UNAVAILABLE" {
		t.Fatalf("result=%+v", result)
	}
}
