package sessionformat

import (
	"encoding/json"
	"errors"
	"fmt"
)

// InboxState is the durable pending-input projection of agent/inbox/spliced.
// The event log remains the only persisted copy of these messages.
type InboxState struct {
	NextTurn []json.RawMessage
	NextStep []json.RawMessage
}

func (s InboxState) Clone() InboxState {
	return InboxState{NextTurn: cloneRawMessages(s.NextTurn), NextStep: cloneRawMessages(s.NextStep)}
}

func cloneRawMessages(messages []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(messages))
	for index, message := range messages {
		out[index] = append(json.RawMessage(nil), message...)
	}
	return out
}

// Next applies one already-framed event without changing the receiver. This
// mirrors DeepSeek's projection: splice positions are exact, and no message
// identity may be pending in both lists or twice in one list.
func (s InboxState) Next(event Event) (InboxState, error) {
	if event.Type != "agent/inbox/spliced" {
		return s, nil
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(event.Data, &raw) != nil || raw == nil {
		return InboxState{}, errors.New("agent inbox splice requires object data")
	}
	var target string
	if json.Unmarshal(raw["target"], &target) != nil || (target != "next-turn" && target != "next-step") {
		return InboxState{}, errors.New("invalid agent inbox target")
	}
	start, err := safeCount(raw["start"])
	if err != nil {
		return InboxState{}, errors.New("invalid agent inbox start")
	}
	removed := uint64(0)
	if value, present := raw["removedCount"]; present {
		removed, err = safeCount(value)
		if err != nil {
			return InboxState{}, errors.New("invalid agent inbox removedCount")
		}
	}
	if value, present := raw["outcome"]; present && string(value) != `"canceled"` {
		return InboxState{}, errors.New("invalid agent inbox outcome")
	}
	var inserted []json.RawMessage
	if json.Unmarshal(raw["inserted"], &inserted) != nil || inserted == nil {
		return InboxState{}, errors.New("agent inbox splice requires inserted array")
	}
	current := s.NextTurn
	if target == "next-step" {
		current = s.NextStep
	}
	if start > uint64(len(current)) || removed > uint64(len(current))-start {
		return InboxState{}, errors.New("agent inbox splice is outside pending messages")
	}
	var moveToNextStep bool
	if value, present := raw["moveToNextStep"]; present {
		if json.Unmarshal(value, &moveToNextStep) != nil || !moveToNextStep || target != "next-turn" ||
			removed != 1 || len(inserted) != 0 || raw["outcome"] != nil {
			return InboxState{}, errors.New("invalid agent inbox move to next step")
		}
	}
	next := make([]json.RawMessage, 0, len(current)-int(removed)+len(inserted))
	next = append(next, cloneRawMessages(current[:start])...)
	next = append(next, cloneRawMessages(inserted)...)
	next = append(next, cloneRawMessages(current[start+removed:])...)
	candidate := s.Clone()
	if target == "next-turn" {
		candidate.NextTurn = next
		if moveToNextStep {
			candidate.NextStep = append(candidate.NextStep, append(json.RawMessage(nil), current[start]...))
		}
	} else {
		candidate.NextStep = next
	}
	ids := make(map[string]bool, len(candidate.NextTurn)+len(candidate.NextStep))
	for _, message := range append(cloneRawMessages(candidate.NextTurn), candidate.NextStep...) {
		var fields struct {
			ID      string            `json:"id"`
			Role    string            `json:"role"`
			Source  json.RawMessage   `json:"source"`
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal(message, &fields) != nil || fields.ID == "" || fields.Role != "user" || len(fields.Source) == 0 || fields.Content == nil {
			return InboxState{}, errors.New("agent inbox contains invalid user message")
		}
		if ids[fields.ID] {
			return InboxState{}, fmt.Errorf("agent inbox message %q is already pending", fields.ID)
		}
		ids[fields.ID] = true
	}
	return candidate, nil
}

func FoldInbox(events []Event) (InboxState, error) {
	var state InboxState
	for _, event := range events {
		next, err := state.Next(event)
		if err != nil {
			return InboxState{}, fmt.Errorf("agent inbox event %d: %w", event.Seq, err)
		}
		state = next
	}
	return state, nil
}
