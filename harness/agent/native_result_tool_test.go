package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go-bot/harness/content"
	"go-bot/harness/session"
	"go-bot/internal/modeldata"
)

type normalizedProbe struct{ failure bool }

func (normalizedProbe) Spec() ToolSpec {
	return ToolSpec{Name: "protocol_result", Parameters: json.RawMessage(`{"type":"object"}`), Timeout: 100 * time.Millisecond}
}
func (normalizedProbe) Validate(json.RawMessage) error { return nil }
func (normalizedProbe) Execute(context.Context, json.RawMessage) (string, error) {
	return "", errors.New("normalized dispatch was bypassed")
}
func (normalizedProbe) ExecutePresented(context.Context, json.RawMessage) (string, json.RawMessage, error) {
	return "", nil, errors.New("normalized dispatch was bypassed")
}
func (p normalizedProbe) ExecuteResult(context.Context, json.RawMessage) (modeldata.ToolResult, error) {
	return modeldata.ToolResult{IsError: p.failure, Code: "PROTOCOL_ERROR", ErrorName: "ProtocolError", ContentBlocks: []content.ContentBlock{{Type: "text", Text: "before"}, {Type: "image", ImageRefID: "sha256:0000000000000000000000000000000000000000000000000000000000000000", MIMEType: "image/png", Width: 2, Height: 2, Bytes: 10}, {Type: "text", Text: "after"}}, Meta: json.RawMessage(`{"protocol":"retained"}`)}, nil
}

func TestNormalizedProtocolFailureKeepsItsOrderedContentAndMetadata(t *testing.T) {
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "protocol", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(live, &nativeAnswerModel{}, "p", "m", "prompt", []Tool{normalizedProbe{failure: true}}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	result := loop.executeNativeTool(t.Context(), 1, 1, modeldata.ToolRequest{ID: "call", Name: "protocol_result", Arguments: `{}`})
	if !result.IsError || len(result.ContentBlocks) != 3 || result.ContentBlocks[1].Type != "image" || len(result.Meta) == 0 {
		t.Fatalf("normalized failure=%+v", result)
	}
}

func TestToolRequestTimeoutStartsAfterOrdinaryApproval(t *testing.T) {
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "approval-budget", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(live, &nativeAnswerModel{}, "p", "m", "prompt", []Tool{normalizedProbe{}}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	loop.SetToolPreExecutePolicy(func(context.Context, ToolExecution) (PreToolDecision, error) {
		return PreToolDecision{Kind: "ask"}, nil
	})
	loop.SetToolPolicyApprover(func(ctx context.Context, _ ToolExecution, _ ToolPolicyAsk) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return nil
		}
	})
	result := loop.executeNativeTool(t.Context(), 1, 1, modeldata.ToolRequest{ID: "call", Name: "protocol_result", Arguments: `{}`})
	if result.IsError {
		t.Fatalf("tool deadline incorrectly bounded user approval: %+v", result)
	}
}
