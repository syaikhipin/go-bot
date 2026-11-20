package plugins

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"go-bot/harness/agent"
	"go-bot/harness/interaction"
)

func (r *hostBridge) questionCall(ctx context.Context, owner, token string, raw json.RawMessage) (any, error) {
	value, ok := r.invocations.Load(token)
	if !ok {
		return nil, failure("UNAVAILABLE", "An active tool body is required to ask a question")
	}
	live := value.(*hostLiveCall)
	if live.owner != owner {
		return nil, failure("INVALID_ARGUMENT", "The invocation belongs to another plugin")
	}
	questions := agent.ToolQuestionsFromContext(live.ctx)
	if questions == nil {
		return nil, failure("UNAVAILABLE", "This invocation has no user question channel")
	}
	var input struct {
		Questions []interaction.Question `json:"questions"`
	}
	if err := decodeHostJSON(raw, &input); err != nil {
		return nil, err
	}
	if len(input.Questions) > 16 {
		return nil, failure("INVALID_ARGUMENT", "A question request supports at most 16 items")
	}
	if err := interaction.ValidateQuestions(input.Questions); err != nil {
		return nil, failure("INVALID_ARGUMENT", err.Error())
	}
	turn, step, ok := agent.ToolPositionFromContext(live.ctx)
	if !ok {
		return nil, failure("UNAVAILABLE", "The question requires a running Agent step")
	}
	callCtx, cancel := context.WithCancel(live.ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	// Multiple questions from one tool are separate foreground waits. The ID is
	// only an answer-channel key, never a replacement for the durable tool call.
	id := "plugin-question-" + uuid.NewString()
	return questions.Ask(callCtx, turn, step, id, input.Questions)
}
