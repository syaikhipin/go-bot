package proactive

import (
	"context"
	"encoding/json"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/tools"
)

// Output is created afresh before each run. The existing structured-output
// tool still owns staging, ConcludeTurn and success-after-durable-settle.
// Keeping this wrapper in the Host avoids changing workflow-child semantics.
type Output struct {
	inner *tools.NativeStructuredOutput
	Now   func() time.Time
}

func NewOutput() (*Output, error) {
	inner, err := tools.NewNativeStructuredOutput(ResultSchema())
	if err != nil {
		return nil, err
	}
	return &Output{inner: inner, Now: time.Now}, nil
}

func (t *Output) Spec() agent.ToolSpec { return t.inner.Spec() }

func (t *Output) Validate(raw json.RawMessage) error {
	_, err := DecodeResult(raw, t.Now())
	return err
}

func (t *Output) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := t.Validate(raw); err != nil {
		return "", err
	}
	return t.inner.Execute(ctx, raw)
}

func (t *Output) SettleResult(id string, success bool) { t.inner.SettleResult(id, success) }

func (t *Output) Captured() (Result, bool) {
	value, ok := t.inner.Captured()
	if !ok {
		return Result{}, false
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return Result{}, false
	}
	result, err := DecodeResult(raw, time.Time{})
	return result, err == nil
}
