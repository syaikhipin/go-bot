package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"go-bot/harness/session"
	"go-bot/internal/modeldata"
)

// executeNativeStepTools owns the upstream ordered prepare / overlapping
// dispatch / ordered complete contract. Completing in order includes policy
// callbacks and final observers, not merely the journal append.
func (a *NativeLoop) executeNativeStepTools(ctx context.Context, turn uint64, step int, calls []modeldata.ToolRequest, toolCount *int) (budgetExceeded, concluded bool, err error) {
	sequences := make([]uint64, len(calls))
	err = runToolBatch(calls, a.maxParallelToolCalls, batchCallbacks{
		parallel: func(call modeldata.ToolRequest) bool { return a.nativeToolParallel(ctx, call) },
		start: func(index int, call modeldata.ToolRequest) (*modeldata.ToolResult, error) {
			*toolCount++
			seq, err := a.appendNativeToolIntent(context.WithoutCancel(ctx), turn, step, call)
			if err != nil {
				return nil, err
			}
			sequences[index] = seq
			if ctx.Err() != nil {
				result := nativeSkippedToolResult(call)
				return &result, nil
			}
			if *toolCount > a.maxToolCalls {
				budgetExceeded = true
				result := nativeBudgetToolResult(call)
				return &result, nil
			}
			return nil, nil
		},
		prepare: func(call modeldata.ToolRequest) *nativePreparedTool {
			return a.prepareNativeTool(ctx, turn, uint64(step), call)
		},
		commit: func(index int, result modeldata.ToolResult, _ *ToolPipelineResult) error {
			if err := a.commitNativeToolResult(turn, step, sequences[index], calls[index], result); err != nil {
				return err
			}
			concluded = concluded || result.ConcludeTurn
			return nil
		},
	})
	return budgetExceeded, concluded, err
}

func (a *nativeTools) nativeToolParallel(ctx context.Context, call modeldata.ToolRequest) (safe bool) {
	defer func() {
		if recover() != nil {
			safe = false
		}
	}()
	if ctx.Err() != nil {
		return false
	}
	tool, schema, err := a.lookupTool(ctx, call.Name)
	if err != nil || tool == nil {
		return false
	}
	classifier, ok := tool.(ConcurrencyTool)
	if !ok {
		return tool.Spec().ParallelSafe
	}
	args := json.RawMessage(call.Arguments)
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if schema == nil || !json.Valid(args) || validateToolParameters(schema, args) != nil || classifier.Validate(args) != nil {
		return false
	}
	safe, err = classifier.IsConcurrencySafe(ctx, args)
	return safe && err == nil
}

func (a *NativeLoop) appendNativeToolIntent(ctx context.Context, turn uint64, step int, call modeldata.ToolRequest) (uint64, error) {
	event, err := a.append("tool/call", map[string]any{
		"turn": turn, "step": step, "callId": call.ID, "name": call.Name, "arguments": call.Arguments,
	}, nil, nil)
	if err != nil {
		return 0, err
	}
	// Once the call row exists in memory, cancellation must not interrupt its
	// checkpoint. Otherwise runClaimed could later close the turn while this
	// call has no durable result. Dispatch checks ctx again after the flush and
	// records a paired skipped result when the turn was cancelled.
	if err := a.session.Flush(context.WithoutCancel(ctx)); err != nil {
		return 0, err
	}
	return event.Seq, nil
}

// logNativeToolOutcome commits the frozen result and applicable built-in
// presentation facts. Output processing and repeat reminders are already settled.
func (a *NativeLoop) logNativeToolOutcome(turn uint64, step int, callSeq uint64, call modeldata.ToolRequest, result modeldata.ToolResult) error {
	var delivery sessionformat.PresentedFiles
	if call.Name == "present" && !result.IsError && len(result.Meta) > 0 {
		if err := json.Unmarshal(result.Meta, &delivery); err != nil {
			return fmt.Errorf("decode present result: %w", err)
		}
		delivery.Turn, delivery.CallID = turn, call.ID
	}
	if err := a.logToolResult(turn, step, callSeq, result); err != nil {
		return err
	}
	if len(delivery.Files) > 0 {
		if _, err := a.append("deliverables/presented", delivery, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (a *NativeLoop) commitNativeToolResult(turn uint64, step int, callSeq uint64, call modeldata.ToolRequest, result modeldata.ToolResult) error {
	if err := a.logNativeToolOutcome(turn, step, callSeq, call, result); err != nil {
		return err
	}
	// Match the upstream batch FIFO: contexts follow the batch's tool results
	// and retain call order even when tool bodies overlap.
	contexts := make([]map[string]any, 0, len(result.AdditionalContexts))
	for _, message := range result.AdditionalContexts {
		var value map[string]any
		if err := json.Unmarshal(message, &value); err != nil {
			return err
		}
		contexts = append(contexts, value)
	}
	if err := a.session.Flush(context.Background()); err != nil {
		return err
	}
	a.repeatReminders = append(a.repeatReminders, contexts...)
	if a.instructionsForStep != nil {
		return a.scanInstructionFileTouches()
	}
	return nil
}

// observeRepeatedToolResult is an ordered PostExecute processor. Result only
// commits its already formed contexts; it never updates the repeat counter.
func (a *NativeLoop) observeRepeatedToolResult(call modeldata.ToolRequest, result *modeldata.ToolResult) {
	// These two results are synthesized by the loop before entering the tool
	// runtime. DeepSeek's guard observes post-execute attempts, including tool
	// denials, but not calls skipped by turn cancellation or our local budget.
	if result.Code == "TOOL_ABORTED_BEFORE_DISPATCH" || result.Code == "TOOL_BUDGET_EXCEEDED" {
		return
	}
	if reminder := a.repeatGuard.observe(call); reminder != nil {
		message, _ := json.Marshal(nativeTextMessage(
			uuid.NewString(), "user", map[string]any{
				"kind": "repeat-tool-reminder", "form": "notice",
				"summary": call.Name + " × " + fmt.Sprint(a.repeatGuard.count),
			}, reminder.Content))
		result.AdditionalContexts = append(result.AdditionalContexts, message)
	}
}

func nativeSkippedToolResult(call modeldata.ToolRequest) modeldata.ToolResult {
	return modeldata.ToolResult{ID: call.ID, Name: call.Name, IsError: true,
		Code: "TOOL_ABORTED_BEFORE_DISPATCH", Content: "Tool call was not dispatched because the turn was cancelled."}
}

func nativeBudgetToolResult(call modeldata.ToolRequest) modeldata.ToolResult {
	return modeldata.ToolResult{ID: call.ID, Name: call.Name, IsError: true,
		Code: "TOOL_BUDGET_EXCEEDED", Content: "Tool call budget exceeded."}
}
