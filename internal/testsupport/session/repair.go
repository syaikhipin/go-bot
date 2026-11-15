package session

import (
	"context"
	"encoding/json"
	"fmt"

	"go-harness/internal/modeldata"
)

const (
	ToolOutcomeUnknown = modeldata.ToolOutcomeUnknown
	ToolNotStarted     = modeldata.ToolNotStarted
	// No model or tool could have run before the first step. Replaying this
	// inbox entry after repair cannot repeat an external effect.
	TurnInterruptedBeforeStep = "interrupted_before_step"
)

// Keep aliases for the retired journal while the production NativeLoop uses
// DTOs that do not import that journal package.
type ToolRequest = modeldata.ToolRequest
type AssistantOutputBlock = modeldata.AssistantOutputBlock
type AssistantMessage = modeldata.AssistantMessage
type ModelUsage = modeldata.ModelUsage

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

type ToolResult = modeldata.ToolResult

type pendingCall struct {
	request ToolRequest
	step    uint64
	started bool
}

// RepairInterrupted mirrors DeepSeek's open-turn repair: only the last open
// turn is closed. Started calls with no durable result get an unknown-outcome
// result, never an automatic retry. Missing calls are marked not-started.
func RepairInterrupted(ctx context.Context, s *Session) (bool, error) {
	events := s.Events()
	var openTurn, openStep uint64
	var activeMessageID string
	var enteredStep, cancelled bool
	var order []string
	pending := map[string]pendingCall{}
	for _, event := range events {
		switch event.Type {
		case "turn/start":
			openTurn, openStep = event.Turn, 0
			activeMessageID = ""
			if len(event.Data) > 0 {
				var source struct {
					MessageID string `json:"messageId"`
				}
				if err := json.Unmarshal(event.Data, &source); err != nil {
					return false, fmt.Errorf("decode turn source %d: %w", event.Seq, err)
				}
				activeMessageID = source.MessageID
			}
			enteredStep, cancelled = false, false
			order = nil
			pending = map[string]pendingCall{}
		case "turn/end":
			openTurn, openStep = 0, 0
			activeMessageID = ""
			order = nil
			pending = map[string]pendingCall{}
		case "step/start":
			openStep = event.Step
			enteredStep = true
		case "compaction/start":
			if openTurn != 0 {
				enteredStep = true // an auxiliary model request may follow this checkpoint
			}
		case "turn/cancel":
			if openTurn != 0 && event.Turn == openTurn {
				cancelled = true
			}
		case "step/end":
			openStep = 0
			order = nil
			pending = map[string]pendingCall{}
		case "assistant/message":
			var message AssistantMessage
			if err := json.Unmarshal(event.Data, &message); err != nil {
				return false, fmt.Errorf("decode assistant event %d: %w", event.Seq, err)
			}
			for _, call := range message.ToolCalls {
				if call.ID == "" {
					return false, fmt.Errorf("assistant event %d has empty tool call ID", event.Seq)
				}
				if _, exists := pending[call.ID]; !exists {
					order = append(order, call.ID)
				}
				pending[call.ID] = pendingCall{request: call, step: event.Step}
			}
		case "tool/call":
			var call ToolCall
			if err := json.Unmarshal(event.Data, &call); err != nil {
				return false, fmt.Errorf("decode tool call %d: %w", event.Seq, err)
			}
			entry, exists := pending[call.ID]
			if !exists {
				order = append(order, call.ID)
				entry = pendingCall{request: ToolRequest{ID: call.ID, Name: call.Name, Arguments: call.Arguments}, step: event.Step}
			}
			entry.started = true
			pending[call.ID] = entry
		case "tool/result":
			var result ToolResult
			if err := json.Unmarshal(event.Data, &result); err != nil {
				return false, fmt.Errorf("decode tool result %d: %w", event.Seq, err)
			}
			delete(pending, result.ID)
		}
	}
	if openTurn == 0 {
		return false, nil
	}
	for _, id := range order {
		call, exists := pending[id]
		if !exists {
			continue
		}
		result := ToolResult{ID: id, Name: call.request.Name, IsError: true}
		if call.started {
			result.Code = ToolOutcomeUnknown
			result.Content = "Tool call started, but no result was durably recorded. Inspect external state before retrying any side effect."
		} else {
			result.Code = ToolNotStarted
			result.Content = "Tool call was requested but not recorded as started."
		}
		if _, err := s.Append("tool/result", openTurn, call.step, result); err != nil {
			return false, err
		}
	}
	if openStep != 0 {
		if _, err := s.Append("step/end", openTurn, openStep, map[string]string{"reason": "interrupted"}); err != nil {
			return false, err
		}
	}
	reason := "interrupted"
	if activeMessageID != "" && !enteredStep && !cancelled {
		reason = TurnInterruptedBeforeStep
	}
	if _, err := s.Append("turn/end", openTurn, 0, map[string]string{"reason": reason}); err != nil {
		return false, err
	}
	if err := s.Flush(ctx); err != nil {
		return false, err
	}
	return true, nil
}
