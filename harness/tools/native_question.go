package tools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/interaction"
	"go-bot/harness/plan"
)

// NativeAskUserQuestion uses the upstream timed tool shape. The v4 log
// contains its tool/call and tool/result, never custom question events.
type NativeAskUserQuestion struct{ Questions *interaction.NativeQuestions }

func (t NativeAskUserQuestion) Spec() agent.ToolSpec { return askUserQuestionSpec() }

func (t NativeAskUserQuestion) Validate(raw json.RawMessage) error {
	if t.Questions == nil {
		return interaction.ErrUnavailable
	}
	_, _, err := decodeAskQuestionArgs(raw)
	return err
}

func (t NativeAskUserQuestion) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := t.Validate(raw); err != nil {
		return "", err
	}
	questions, timeout, err := decodeAskQuestionArgs(raw)
	if err != nil {
		return "", err
	}
	turn, step, ok := agent.ToolPositionFromContext(ctx)
	if !ok {
		return "", errors.New("ask_user_question requires an active Agent tool call")
	}
	callID, ok := agent.ToolCallIDFromContext(ctx)
	if !ok {
		return "", errors.New("ask_user_question requires a durable tool-call identity")
	}
	if timeout == -1 {
		answer, err := t.Questions.Ask(ctx, turn, step, callID, questions)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(answer)
		return string(encoded), err
	}
	answer, pending, err := t.Questions.AskTimed(ctx, turn, step, callID, questions, time.Duration(timeout)*time.Second)
	if err != nil {
		return "", err
	}
	if pending {
		encoded, err := json.Marshal(map[string]any{
			"pending": true, "callId": callID,
			"message": "No answer batch arrived before the timeout. This is pending, not a skipped answer. Continue useful independent work. The user can still answer; their reply will be a user message identified as answer_to_pending_question with this callId and the original questions. Do not treat this as permission.",
		})
		return string(encoded), err
	}
	encoded, err := json.Marshal(answer)
	return string(encoded), err
}

type NativeExitPlanMode struct {
	Plan      *plan.NativeService
	Questions *interaction.NativeQuestions
}

func (t NativeExitPlanMode) Spec() agent.ToolSpec { return exitPlanModeSpec() }

func (t NativeExitPlanMode) Validate(raw json.RawMessage) error {
	_, err := decodePlan(raw)
	return err
}

func (t NativeExitPlanMode) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	text, err := decodePlan(raw)
	if err != nil {
		return "", err
	}
	if t.Plan == nil || t.Questions == nil {
		return "", interaction.ErrUnavailable
	}
	view, err := t.Plan.View()
	if err != nil {
		return "", err
	}
	if !view.Active {
		return "", errors.New("exit_plan_mode is only available in plan mode")
	}
	turn, step, ok := agent.ToolPositionFromContext(ctx)
	if !ok {
		return "", errors.New("exit_plan_mode requires an active Agent tool call")
	}
	callID, ok := agent.ToolCallIDFromContext(ctx)
	if !ok {
		return "", errors.New("exit_plan_mode requires a durable tool-call identity")
	}
	answer, err := t.Questions.Ask(ctx, turn, step, callID, []interaction.Question{{
		ID: "plan-review", Header: "Plan review", Question: "Approve this plan and leave plan mode?", Detail: text,
		Options: []interaction.Option{
			{Label: "Approve", Description: "Leave plan mode; the plan is carried out from the next step."},
			{Label: "Keep planning", Description: "Stay in plan mode; feedback goes back to the model."},
		},
	}})
	if errors.Is(err, interaction.ErrCancelled) {
		agent.ConcludeTurn(ctx)
		return `{"approved":false}`, nil
	}
	if err != nil {
		return "", err
	}
	if len(answer.Answers) != 1 || answer.Answers[0].ID != "plan-review" ||
		len(answer.Answers[0].Selected) != 1 || answer.Answers[0].Selected[0] != "Approve" ||
		answer.Answers[0].Custom != nil {
		if len(answer.Answers) == 1 && answer.Answers[0].Custom != nil && *answer.Answers[0].Custom != "" {
			return "", errors.New("The user chose to keep planning; their feedback: " + *answer.Answers[0].Custom)
		}
		return "", errors.New("The user chose to keep planning; revise the plan and present it again.")
	}
	if err := t.Plan.Queue(false); err != nil {
		return "", err
	}
	return `{"approved":true}`, nil
}
