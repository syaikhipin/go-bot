package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const GoalChangeVersion = 1

var goalBlockCode = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

type GoalRef struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
}

type GoalBlockReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type GoalSnapshot struct {
	ID            string           `json:"id"`
	Revision      int              `json:"revision"`
	Objective     string           `json:"objective"`
	Phase         string           `json:"phase"`
	MaxGoalRounds int              `json:"maxGoalRounds"`
	BlockedReason *GoalBlockReason `json:"blockedReason,omitempty"`
}

type GoalChange struct {
	Kind          string        `json:"kind"`
	Version       int           `json:"version"`
	Operation     string        `json:"operation"`
	Goal          *GoalSnapshot `json:"goal,omitempty"`
	RoundsStarted int           `json:"roundsStarted"`
	CreatedAt     int64         `json:"createdAt"`
	UpdatedAt     int64         `json:"updatedAt"`
	Cleared       *GoalRef      `json:"cleared,omitempty"`
	ClearedAt     int64         `json:"clearedAt,omitempty"`
}

type GoalState struct {
	Goal          GoalSnapshot `json:"goal"`
	RoundsStarted int          `json:"roundsStarted"`
	CreatedAt     int64        `json:"createdAt"`
	UpdatedAt     int64        `json:"updatedAt"`
}

type goalProjection struct {
	processed int
	current   *GoalState
	seen      map[string]bool
	err       error
}

func cloneGoalState(state GoalState) GoalState {
	if state.Goal.BlockedReason != nil {
		reason := *state.Goal.BlockedReason
		state.Goal.BlockedReason = &reason
	}
	return state
}

func decodeGoalChange(raw json.RawMessage) (GoalChange, error) {
	var change GoalChange
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return change, err
	}
	if keys == nil {
		return change, errors.New("goal change must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&change); err != nil {
		return change, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return change, errors.New("goal change must contain one JSON object")
	}
	if change.Kind != "goal/change" || change.Version != GoalChangeVersion {
		return change, errors.New("invalid goal change kind or version")
	}
	if change.Operation == "clear" {
		if !goalKeysEqual(keys, "kind", "version", "operation", "cleared", "clearedAt") || change.Cleared == nil || change.Cleared.ID == "" || change.Cleared.Revision < 1 || change.ClearedAt < 0 {
			return change, errors.New("invalid goal clear tombstone")
		}
		return change, nil
	}
	if !goalKeysEqual(keys, "kind", "version", "operation", "goal", "roundsStarted", "createdAt", "updatedAt") {
		return change, errors.New("goal snapshot change has unexpected or missing fields")
	}
	switch change.Operation {
	case "create", "edit", "pause", "resume", "complete", "block":
	default:
		return change, errors.New("unknown goal operation")
	}
	if change.Goal == nil || change.Goal.ID == "" || change.Goal.Revision < 1 || change.Goal.MaxGoalRounds < 1 || change.Goal.Objective == "" || strings.TrimSpace(change.Goal.Objective) != change.Goal.Objective || change.RoundsStarted < 0 || change.RoundsStarted > change.Goal.MaxGoalRounds || change.CreatedAt < 0 || change.UpdatedAt < change.CreatedAt {
		return change, errors.New("invalid goal snapshot fields")
	}
	switch change.Goal.Phase {
	case "active", "paused", "complete":
		if change.Goal.BlockedReason != nil {
			return change, errors.New("non-blocked goal carries blocker")
		}
	case "blocked":
		if change.Goal.BlockedReason == nil || !goalBlockCode.MatchString(change.Goal.BlockedReason.Code) || change.Goal.BlockedReason.Message == "" || strings.TrimSpace(change.Goal.BlockedReason.Message) != change.Goal.BlockedReason.Message {
			return change, errors.New("invalid goal blocker")
		}
	default:
		return change, errors.New("invalid goal phase")
	}
	return change, nil
}

func goalKeysEqual(fields map[string]json.RawMessage, required ...string) bool {
	if len(fields) != len(required) {
		return false
	}
	for _, key := range required {
		if _, exists := fields[key]; !exists {
			return false
		}
	}
	return true
}

func (p *goalProjection) apply(event Event) error {
	switch event.Type {
	case "goal/change":
		change, err := decodeGoalChange(event.Data)
		if err != nil {
			return fmt.Errorf("goal change at event %d: %w", event.Seq, err)
		}
		if err := p.applyChange(change); err != nil {
			return fmt.Errorf("goal change at event %d: %w", event.Seq, err)
		}
	case "user/message":
		var message struct {
			Source *struct {
				Kind     string `json:"kind"`
				GoalID   string `json:"goalId"`
				Revision int    `json:"revision"`
				Round    int    `json:"round"`
			} `json:"source"`
		}
		if err := json.Unmarshal(event.Data, &message); err != nil {
			return fmt.Errorf("goal message at event %d: %w", event.Seq, err)
		}
		if message.Source == nil || message.Source.Kind != "goal" {
			return nil
		}
		current := p.current
		if current == nil || current.Goal.Phase != "active" || message.Source.GoalID != current.Goal.ID || message.Source.Revision != current.Goal.Revision || message.Source.Round != current.RoundsStarted+1 || message.Source.Round > current.Goal.MaxGoalRounds {
			return fmt.Errorf("goal message at event %d is not the next admitted round", event.Seq)
		}
		current.RoundsStarted = message.Source.Round
	}
	return nil
}

func (p *goalProjection) applyChange(change GoalChange) error {
	if change.Operation == "clear" {
		if p.current == nil || change.Cleared.ID != p.current.Goal.ID || change.Cleared.Revision != p.current.Goal.Revision+1 || change.ClearedAt < p.current.UpdatedAt {
			return errors.New("clear must advance the current goal")
		}
		p.current = nil
		return nil
	}
	next := *change.Goal
	if change.Operation == "create" {
		if next.Revision != 1 || next.Phase != "active" || change.RoundsStarted != 0 || (p.current != nil && p.current.Goal.Phase != "complete") || p.seen[next.ID] {
			return errors.New("create requires a fresh active goal")
		}
		if p.seen == nil {
			p.seen = make(map[string]bool)
		}
		p.seen[next.ID] = true
		p.current = &GoalState{Goal: next, RoundsStarted: 0, CreatedAt: change.CreatedAt, UpdatedAt: change.UpdatedAt}
		return nil
	}
	current := p.current
	if current == nil || next.ID != current.Goal.ID || next.Revision != current.Goal.Revision+1 || change.RoundsStarted != current.RoundsStarted || change.CreatedAt != current.CreatedAt || change.UpdatedAt < current.UpdatedAt {
		return errors.New("goal change must advance current revision and preserve counters")
	}
	if change.Operation != "edit" && (next.Objective != current.Goal.Objective || next.MaxGoalRounds != current.Goal.MaxGoalRounds) {
		return errors.New("non-edit goal change altered definition")
	}
	switch change.Operation {
	case "edit":
		if next.Phase != current.Goal.Phase || !sameGoalBlockReason(next.BlockedReason, current.Goal.BlockedReason) {
			return errors.New("edit changed goal phase or blocker")
		}
	case "pause":
		if current.Goal.Phase != "active" || next.Phase != "paused" {
			return errors.New("pause requires active goal")
		}
	case "resume":
		if (current.Goal.Phase != "active" && current.Goal.Phase != "paused" && current.Goal.Phase != "blocked") || next.Phase != "active" || current.RoundsStarted >= next.MaxGoalRounds {
			return errors.New("resume requires a resumable goal with remaining rounds")
		}
	case "complete":
		if current.Goal.Phase == "complete" || next.Phase != "complete" {
			return errors.New("complete requires an unfinished goal")
		}
	case "block":
		if current.Goal.Phase != "active" || next.Phase != "blocked" {
			return errors.New("block requires active goal")
		}
	}
	p.current = &GoalState{Goal: next, RoundsStarted: change.RoundsStarted, CreatedAt: change.CreatedAt, UpdatedAt: change.UpdatedAt}
	return nil
}

func sameGoalBlockReason(a, b *GoalBlockReason) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (s *Session) projectGoal() error {
	if s.goal.err != nil {
		return s.goal.err
	}
	for s.goal.processed < s.durable {
		if err := s.goal.apply(s.events[s.goal.processed]); err != nil {
			s.goal.err = err
			return err
		}
		s.goal.processed++
	}
	return nil
}

func (s *Session) ValidateGoal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectGoal()
}

// CurrentGoal reads the durable Goal projection. Process-local continuation
// activation deliberately does not appear here or survive cold resume.
func (s *Session) CurrentGoal() (GoalState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.projectGoal(); err != nil {
		return GoalState{}, false, err
	}
	if s.goal.current == nil {
		return GoalState{}, false, nil
	}
	return cloneGoalState(*s.goal.current), true, nil
}
