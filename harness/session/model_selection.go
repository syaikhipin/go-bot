package sessionformat

import (
	"encoding/json"
	"errors"
	"time"
)

// ModelSelection is an explicit route choice, not a resolved adapter config.
// Empty effort means omission; adapter-defaulted effort is never pinned here.
type ModelSelection struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

type ModelSelectionState struct {
	LastUsed *ModelSelection `json:"lastUsed"`
	Pending  *ModelSelection `json:"pending"`
}

func (s ModelSelectionState) Current() *ModelSelection {
	if s.Pending != nil {
		copy := *s.Pending
		return &copy
	}
	if s.LastUsed != nil {
		copy := *s.LastUsed
		return &copy
	}
	return nil
}

func sameModelSelection(left, right *ModelSelection) bool {
	return left == right || left != nil && right != nil && *left == *right
}

func (s ModelSelectionState) Next(event Event) (ModelSelectionState, error) {
	switch event.Type {
	case "model/selection":
		var selected *ModelSelection
		if err := json.Unmarshal(event.Data, &selected); err != nil {
			return s, errors.New("invalid model selection")
		}
		if selected != nil && (selected.Provider == "" || selected.Model == "") {
			return s, errors.New("model selection requires provider and model")
		}
		// Null clears a pending choice; it does not erase the last request.
		s.Pending = selected
	case "request/header":
		var value struct {
			Header struct {
				Config          ModelSelection
				AdapterDefaults struct{ ReasoningEffort bool }
			}
		}
		if json.Unmarshal(event.Data, &value) != nil || value.Header.Config.Provider == "" || value.Header.Config.Model == "" {
			return s, errors.New("request header requires provider and model")
		}
		selected := value.Header.Config
		if value.Header.AdapterDefaults.ReasoningEffort {
			selected.ReasoningEffort = ""
		}
		s.LastUsed = &selected
		if sameModelSelection(s.Pending, s.LastUsed) {
			s.Pending = nil
		}
	}
	return s, nil
}

func FoldModelSelection(events []Event) (ModelSelectionState, error) {
	var state ModelSelectionState
	for _, event := range events {
		next, err := state.Next(event)
		if err != nil {
			return ModelSelectionState{}, err
		}
		state = next
	}
	return state, nil
}

// ModelSelection reads the original live log, advancing only the missing
// suffix. Its state is process-local derived data, never a snapshot file.
func (s *LiveSession) ModelSelection() (ModelSelectionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.modelSelectionLocked()
	return ModelSelectionState{LastUsed: cloneSelection(state.LastUsed), Pending: cloneSelection(state.Pending)}, err
}

func cloneSelection(value *ModelSelection) *ModelSelection {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (s *LiveSession) modelSelectionLocked() (ModelSelectionState, error) {
	for s.modelSelectionCursor < len(s.events) {
		next, err := s.modelSelection.Next(s.events[s.modelSelectionCursor])
		if err != nil {
			return ModelSelectionState{}, err
		}
		s.modelSelection = next
		s.modelSelectionCursor++
	}
	return s.modelSelection, nil
}

// SelectModel appends synchronously through the existing writer's live log.
// Only an equal recorded selection is a no-op; an options-only fallback still
// needs its first durable fact. Checkpoint/Flush remains the writer's policy.
func (s *LiveSession) SelectModel(selection ModelSelection) error {
	if selection.Provider == "" || selection.Model == "" {
		return errors.New("model selection requires provider and model")
	}
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.modelSelectionLocked()
	if err != nil || sameModelSelection(state.Current(), &selection) {
		return err
	}
	data, _ := json.Marshal(selection)
	_, err = s.appendInputsLocked([]EventInput{{Type: "model/selection", Data: data}}, time.Now().UnixMilli())
	return err
}
