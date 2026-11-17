package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"go-bot/harness/agent"
)

const NativeStructuredOutputInstruction = "When you have your final answer, you MUST report it by calling the `structured_output` tool with arguments matching its parameter schema exactly. Do not finish with a plain text answer: only the tool call counts as your result."

// NativeStructuredOutput is scoped to one workflow child activation. It is
// never installed on a resumed child or shared across sibling Sessions.
type NativeStructuredOutput struct {
	schema json.RawMessage
	mu     sync.Mutex
	value  json.RawMessage
}

func NewNativeStructuredOutput(schema map[string]any) (*NativeStructuredOutput, error) {
	if schema == nil || schema["type"] != "object" {
		return nil, errors.New("structured output requires an object-rooted schema")
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	return &NativeStructuredOutput{schema: raw}, nil
}

func (t *NativeStructuredOutput) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "structured_output",
		Description: "Report your final structured result. Call this exactly once when your answer is complete; the arguments must match this tool's parameter schema exactly.",
		Parameters:  append(json.RawMessage(nil), t.schema...),
	}
}

func (*NativeStructuredOutput) Validate(json.RawMessage) error { return nil }

func (t *NativeStructuredOutput) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.value != nil {
		return "", errors.New("structured output was already recorded")
	}
	t.value = append(json.RawMessage(nil), raw...)
	agent.ConcludeTurn(ctx)
	return "Structured output recorded.", nil
}

func (t *NativeStructuredOutput) Captured() (any, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.value == nil {
		return nil, false
	}
	var value any
	if json.Unmarshal(t.value, &value) != nil {
		return nil, false
	}
	return value, true
}
