package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// PlanMode is a control-plane fact, not a model-facing transcript message.
// The latest durable plan/mode event determines the active policy on resume.
type PlanMode struct {
	Active bool `json:"active"`
}

type planProjection struct {
	processed int
	active    bool
	err       error
}

func (s *Session) projectPlan() error {
	if s.plan.err != nil {
		return s.plan.err
	}
	for s.plan.processed < s.durable {
		event := s.events[s.plan.processed]
		if event.Type == "plan/mode" {
			var mode PlanMode
			var fields map[string]json.RawMessage
			activeRaw := []byte(nil)
			if err := json.Unmarshal(event.Data, &fields); err == nil {
				activeRaw = bytes.TrimSpace(fields["active"])
			}
			if len(fields) != 1 || (!bytes.Equal(activeRaw, []byte("true")) && !bytes.Equal(activeRaw, []byte("false"))) {
				s.plan.err = fmt.Errorf("invalid plan mode at event %d", event.Seq)
				return s.plan.err
			}
			decoder := json.NewDecoder(bytes.NewReader(event.Data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&mode); err != nil {
				s.plan.err = fmt.Errorf("invalid plan mode at event %d: %w", event.Seq, err)
				return s.plan.err
			}
			if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
				s.plan.err = fmt.Errorf("invalid trailing plan mode data at event %d", event.Seq)
				return s.plan.err
			}
			s.plan.active = mode.Active
		}
		s.plan.processed++
	}
	return nil
}

func (s *Session) ValidatePlan() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectPlan()
}

func (s *Session) CurrentPlanMode() (PlanMode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.projectPlan(); err != nil {
		return PlanMode{}, err
	}
	return PlanMode{Active: s.plan.active}, nil
}
