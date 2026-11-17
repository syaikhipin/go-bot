package agent

import (
	"context"
	"errors"

	"go-bot/internal/modeldata"
)

// Provider implementations run in the same Go process as the Harness. Keep
// their panics within the attempt boundary so the logged request can settle
// as a failure and the owner can continue serving later turns.
func completeModel(model Model, ctx context.Context, messages []Message, tools []ToolSpec) (response modeldata.AssistantMessage, err error) {
	defer func() {
		if recover() != nil {
			response = modeldata.AssistantMessage{}
			err = errors.New("model adapter panicked")
		}
	}()
	return model.Complete(ctx, messages, tools)
}

func summarizeModel(model Summarizer, ctx context.Context, messages []Message, tools []ToolSpec) (response SummaryResult, err error) {
	defer func() {
		if recover() != nil {
			response = SummaryResult{}
			err = errors.New("model summarizer panicked")
		}
	}()
	return model.Summarize(ctx, messages, tools)
}
