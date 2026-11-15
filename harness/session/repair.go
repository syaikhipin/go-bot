package sessionformat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const interruptedStartedToolText = "The tool call was interrupted after it was recorded, but no result was durably recorded. Its outcome is unknown. Decide whether to retry from the tool semantics: retry only if the operation is read-only or idempotent; if it may have side effects, first verify external state or ask the user. Do not retry blindly."
const interruptedNotStartedToolText = "The tool call was interrupted before the Harness recorded it as started. Retry it if it is still needed."

type pendingV4Tool struct {
	id      string
	turn    uint64
	step    uint64
	callSeq *uint64
	done    bool
}

// RepairInterruptedTurn appends the same conservative closer sequence as
// DeepSeek's interruptedTurnClosers: unanswered tool results in assistant
// order, then an open step/end and turn/end(interrupted). It never reruns a
// tool. Synthetic events reuse the last observed timestamp.
func RepairInterruptedTurn(ctx context.Context, s *LiveSession) (bool, error) {
	if s == nil {
		return false, errors.New("Session v4 is required for interrupted-turn repair")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	repaired, err := appendInterruptedTurnClosers(s)
	if err != nil || !repaired {
		return repaired, err
	}
	if err := s.Flush(context.Background()); err != nil {
		return false, err
	}
	return true, nil
}

// Restoration adds the resume marker after these closers. Keep the append
// separate from Flush so the closers and marker share one checkpoint.
func appendInterruptedTurnClosers(s *LiveSession) (bool, error) {
	events := s.Events()
	if err := ValidateV4Relationships(events); err != nil {
		return false, fmt.Errorf("invalid Session v4 before repair: %w", err)
	}
	var openTurn, openStep uint64
	var pending []pendingV4Tool
	positions := make(map[string]int)
	clearPending := func() {
		pending = nil
		clear(positions)
	}
	for _, event := range events {
		data, err := objectValue(event.Data)
		if err != nil && (event.Type == "turn/start" || event.Type == "turn/end" || event.Type == "step/start" || event.Type == "step/end" || event.Type == "assistant/message" || event.Type == "tool/call" || event.Type == "tool/result") {
			return false, err
		}
		switch event.Type {
		case "turn/start":
			openTurn, _ = safeCount(data["turn"])
			openStep = 0
			clearPending()
		case "turn/end":
			openTurn, openStep = 0, 0
			clearPending()
		case "step/start":
			openStep, _ = safeCount(data["step"])
		case "step/end":
			openStep = 0
			clearPending()
		case "assistant/message":
			message, err := objectValue(data["message"])
			if err != nil {
				return false, err
			}
			var blocks []json.RawMessage
			if json.Unmarshal(message["content"], &blocks) != nil {
				return false, errors.New("assistant message has invalid content")
			}
			turn, _ := safeCount(data["turn"])
			step, _ := safeCount(data["step"])
			for _, raw := range blocks {
				block, err := objectValue(raw)
				if err != nil || !bytes.Equal(block["type"], []byte(`"tool-call"`)) {
					continue
				}
				id, err := requiredString(block["id"])
				if err != nil {
					return false, err
				}
				positions[id] = len(pending)
				pending = append(pending, pendingV4Tool{id: id, turn: turn, step: step})
			}
		case "tool/call":
			id, err := requiredString(data["callId"])
			if err != nil {
				return false, err
			}
			if index, found := positions[id]; found {
				seq := event.Seq
				pending[index].callSeq = &seq
			}
		case "tool/result":
			if !bytes.Equal(bytes.TrimSpace(event.SurfaceOp), []byte(`"append"`)) {
				continue
			}
			message, err := objectValue(data["message"])
			if err != nil {
				return false, err
			}
			id, err := requiredString(message["toolCallId"])
			if err != nil {
				return false, err
			}
			if index, found := positions[id]; found {
				pending[index].done = true
			}
		}
	}
	if openTurn == 0 {
		return false, nil
	}
	if len(events) == 0 {
		return false, errors.New("open Session v4 turn has no events")
	}
	timestamp := events[len(events)-1].Time
	nextSeq := uint64(len(events))
	appendSynthetic := func(eventType string, data any, surfaceOp json.RawMessage, sources []uint64) error {
		encoded, err := json.Marshal(data)
		if err != nil {
			return err
		}
		_, err = s.appendAt(EventInput{Type: eventType, Data: encoded, SurfaceOp: surfaceOp, SourceEventSeqs: sources}, timestamp)
		if err == nil {
			nextSeq++
		}
		return err
	}
	for _, tool := range pending {
		if tool.done {
			continue
		}
		seq := nextSeq
		started := tool.callSeq != nil
		code, name, messageText := "TOOL_NOT_STARTED", "ToolNotStartedError", interruptedNotStartedToolText
		var sources []uint64
		if started {
			code, name, messageText = "TOOL_OUTCOME_UNKNOWN", "ToolOutcomeUnknownError", interruptedStartedToolText
			sources = []uint64{*tool.callSeq}
		}
		message := map[string]any{
			"id":   fmt.Sprintf("interrupted-tool-result-%s-%d", tool.id, seq),
			"role": "tool", "toolCallId": tool.id, "isError": true,
			"source":  map[string]any{"kind": "tool", "callId": tool.id},
			"content": []any{map[string]any{"type": "text", "text": messageText}},
		}
		if err := appendSynthetic("tool/result", map[string]any{"turn": tool.turn, "step": tool.step, "message": message,
			"error": map[string]any{"name": name, "code": code}}, json.RawMessage(`"append"`), sources); err != nil {
			return false, poisonRepair(s, err)
		}
	}
	if openStep != 0 {
		if err := appendSynthetic("step/end", map[string]any{"turn": openTurn, "step": openStep}, nil, nil); err != nil {
			return false, poisonRepair(s, err)
		}
	}
	if err := appendSynthetic("turn/end", map[string]any{"turn": openTurn, "reason": map[string]any{"kind": "interrupted"}}, nil, nil); err != nil {
		return false, poisonRepair(s, err)
	}
	return true, nil
}

func poisonRepair(s *LiveSession, err error) error {
	s.mu.Lock()
	s.poison = true
	s.mu.Unlock()
	return fmt.Errorf("Session v4 interrupted-turn repair failed: %w", err)
}
