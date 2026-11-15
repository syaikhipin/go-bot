package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go-harness/harness/session"
	"go-harness/internal/modeldata"
)

// executeNativeStepTools overlaps only consecutive calls whose static tool
// specs declare parallel-safe execution. An exclusive call waits for the pool
// to drain. Dispatch can overlap, while intent and result commits stay in
// model order and each intent is flushed before its tool can have effects.
func (a *NativeLoop) executeNativeStepTools(
	ctx context.Context, turn uint64, step int, calls []modeldata.ToolRequest, toolCount *int,
) (budgetExceeded, concluded bool, err error) {
	for index := 0; index < len(calls); {
		if a.maxParallelToolCalls > 1 && a.parallelSafe[calls[index].Name] && ctx.Err() == nil {
			end := index + 1
			for end < len(calls) && a.parallelSafe[calls[end].Name] {
				end++
			}
			over, done, groupErr := a.executeNativeParallelGroup(ctx, turn, step, calls[index:end], toolCount)
			budgetExceeded = budgetExceeded || over
			concluded = concluded || done
			if groupErr != nil {
				return budgetExceeded, concluded, groupErr
			}
			index = end
			continue
		}
		call := calls[index]
		index++
		*toolCount++
		callSeq, appendErr := a.appendNativeToolIntent(ctx, turn, step, call)
		if appendErr != nil {
			return budgetExceeded, concluded, appendErr
		}
		var result modeldata.ToolResult
		switch {
		case ctx.Err() != nil:
			result = nativeSkippedToolResult(call)
		case *toolCount > a.maxToolCalls:
			budgetExceeded = true
			result = nativeBudgetToolResult(call)
		default:
			result = a.executeNativeTool(ctx, turn, uint64(step), call)
		}
		concluded = concluded || result.ConcludeTurn
		if commitErr := a.commitNativeToolResult(turn, step, callSeq, call, result); commitErr != nil {
			return budgetExceeded, concluded, commitErr
		}
	}
	return budgetExceeded, concluded, nil
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

func (a *NativeLoop) commitNativeToolResult(turn uint64, step int, callSeq uint64, call modeldata.ToolRequest, result modeldata.ToolResult) error {
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
	if err := a.session.Flush(context.Background()); err != nil {
		return err
	}
	// These two results are synthesized by the loop before entering the tool
	// runtime. DeepSeek's guard observes post-execute attempts, including tool
	// denials, but not calls skipped by turn cancellation or our local budget.
	if result.Code == "TOOL_ABORTED_BEFORE_DISPATCH" || result.Code == "TOOL_BUDGET_EXCEEDED" {
		return nil
	}
	if reminder := a.repeatGuard.observe(call); reminder != nil {
		a.repeatReminders = append(a.repeatReminders, nativeTextMessage(
			uuid.NewString(), "user", map[string]any{
				"kind": "repeat-tool-reminder", "form": "notice",
				"summary": call.Name + " × " + fmt.Sprint(a.repeatGuard.count),
			}, reminder.Content))
	}
	return nil
}

func nativeSkippedToolResult(call modeldata.ToolRequest) modeldata.ToolResult {
	return modeldata.ToolResult{ID: call.ID, Name: call.Name, IsError: true,
		Code: "TOOL_ABORTED_BEFORE_DISPATCH", Content: "Tool call was not dispatched because the turn was cancelled."}
}

func nativeBudgetToolResult(call modeldata.ToolRequest) modeldata.ToolResult {
	return modeldata.ToolResult{ID: call.ID, Name: call.Name, IsError: true,
		Code: "TOOL_BUDGET_EXCEEDED", Content: "Tool call budget exceeded."}
}

func (a *NativeLoop) executeNativeParallelGroup(
	ctx context.Context, turn uint64, step int, calls []modeldata.ToolRequest, toolCount *int,
) (budgetExceeded, concluded bool, err error) {
	type completion struct {
		index  int
		result modeldata.ToolResult
	}
	completed := make(chan completion, len(calls))
	results := make([]modeldata.ToolResult, len(calls))
	sequences := make([]uint64, len(calls))
	ready := make([]bool, len(calls))
	nextStart, nextCommit, running := 0, 0, 0
	// Even a persistence failure must not let the turn close while dispatched
	// tools are still changing the world. Reopen repairs any unpaired intents.
	defer func() {
		for running > 0 {
			<-completed
			running--
		}
	}()
	commitReady := func() error {
		for nextCommit < len(calls) && ready[nextCommit] {
			if err := a.commitNativeToolResult(turn, step, sequences[nextCommit], calls[nextCommit], results[nextCommit]); err != nil {
				return err
			}
			concluded = concluded || results[nextCommit].ConcludeTurn
			nextCommit++
		}
		return nil
	}
	startReady := func() error {
		for nextStart < len(calls) && running < a.maxParallelToolCalls && ctx.Err() == nil {
			index := nextStart
			call := calls[index]
			*toolCount++
			callSeq, err := a.appendNativeToolIntent(ctx, turn, step, call)
			if err != nil {
				return err
			}
			sequences[index] = callSeq
			nextStart++
			switch {
			case ctx.Err() != nil:
				results[index] = nativeSkippedToolResult(call)
				ready[index] = true
			case *toolCount > a.maxToolCalls:
				budgetExceeded = true
				results[index] = nativeBudgetToolResult(call)
				ready[index] = true
			default:
				running++
				go func() {
					completed <- completion{index: index, result: a.executeNativeTool(ctx, turn, uint64(step), call)}
				}()
			}
			if err := commitReady(); err != nil {
				return err
			}
		}
		return nil
	}
	if err := startReady(); err != nil {
		return budgetExceeded, concluded, err
	}
	for running > 0 {
		outcome := <-completed
		running--
		results[outcome.index] = outcome.result
		ready[outcome.index] = true
		if err := commitReady(); err != nil {
			return budgetExceeded, concluded, err
		}
		if err := startReady(); err != nil {
			return budgetExceeded, concluded, err
		}
	}
	if ctx.Err() != nil {
		for nextStart < len(calls) {
			index := nextStart
			call := calls[index]
			*toolCount++
			callSeq, err := a.appendNativeToolIntent(context.Background(), turn, step, call)
			if err != nil {
				return budgetExceeded, concluded, err
			}
			sequences[index] = callSeq
			results[index] = nativeSkippedToolResult(call)
			ready[index] = true
			nextStart++
			if err := commitReady(); err != nil {
				return budgetExceeded, concluded, err
			}
		}
	}
	if nextCommit != len(calls) {
		return budgetExceeded, concluded, errors.New("native parallel scheduler left uncommitted results")
	}
	return budgetExceeded, concluded, nil
}
