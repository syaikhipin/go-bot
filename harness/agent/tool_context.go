package agent

import (
	"context"
	"go-bot/harness/interaction"
)

type toolPositionKey struct{}
type turnSourceKey struct{}
type toolCallIDKey struct{}
type toolControlKey struct{}
type toolQuestionKey struct{}
type toolQuestionsKey struct{}

// ToolQuestionsFromContext is available only inside a real Go tool body. The
// channel belongs to the running Session and is never exported to plugins.
func ToolQuestionsFromContext(ctx context.Context) *interaction.NativeQuestions {
	value, _ := ctx.Value(toolQuestionsKey{}).(*interaction.NativeQuestions)
	return value
}

type toolControl struct{ concludeTurn bool }

type toolPosition struct {
	turn uint64
	step uint64
}

func withToolPosition(ctx context.Context, turn, step uint64) context.Context {
	return context.WithValue(ctx, toolPositionKey{}, toolPosition{turn: turn, step: step})
}

func withTurnSource(ctx context.Context, source *MessageSource) context.Context {
	if source == nil {
		return ctx
	}
	copy := *source
	return context.WithValue(ctx, turnSourceKey{}, copy)
}

func withToolCallID(ctx context.Context, callID string) context.Context {
	return context.WithValue(ctx, toolCallIDKey{}, callID)
}

// ToolQuestionFromContext is the most recent user-authored prompt in the session.
func ToolQuestionFromContext(ctx context.Context) string {
	question, _ := ctx.Value(toolQuestionKey{}).(func() string)
	if question == nil {
		return ""
	}
	return question()
}

// ConcludeTurn asks the AgentLoop to stop after committing the current tool
// batch. A dismissed interactive review uses this to yield to the user's next
// message without another model request.
func ConcludeTurn(ctx context.Context) {
	if control, ok := ctx.Value(toolControlKey{}).(*toolControl); ok {
		control.concludeTurn = true
	}
}

// ToolCallIDFromContext identifies the current tool invocation. Model calls
// have a durable assistant record.
func ToolCallIDFromContext(ctx context.Context) (string, bool) {
	callID, ok := ctx.Value(toolCallIDKey{}).(string)
	return callID, ok && callID != ""
}

// TurnSourceFromContext is the durable provenance of the user message that
// started this tool's current turn. It is absent for unverified direct calls.
func TurnSourceFromContext(ctx context.Context) (MessageSource, bool) {
	source, ok := ctx.Value(turnSourceKey{}).(MessageSource)
	return source, ok
}

// ToolPositionFromContext identifies the step that admitted a tool call. It is
// available to tools bound to the AgentLoop's real open step.
func ToolPositionFromContext(ctx context.Context) (turn, step uint64, ok bool) {
	position, ok := ctx.Value(toolPositionKey{}).(toolPosition)
	if !ok || position.turn == 0 || position.step == 0 {
		return 0, 0, false
	}
	return position.turn, position.step, true
}
