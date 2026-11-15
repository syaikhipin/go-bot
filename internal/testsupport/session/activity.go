package session

import (
	"encoding/json"
	"fmt"
	"time"
)

// ActivitySnapshot is a projection of the durable Session prefix, not a new
// source of truth. Idle is false while any inbox message is unclaimed or a
// turn has no durable end. Since is the last ready/input/completed-turn time.
type ActivitySnapshot struct {
	Since time.Time `json:"since"`
	Idle  bool      `json:"idle"`
}

type activityProjection struct {
	processed       int
	last            time.Time
	queued          map[string]struct{}
	openTurn        bool
	turnID          uint64
	activeMessageID string
	enteredStep     bool
	cancelled       bool
	err             error
}

func (p *activityProjection) apply(event Event) error {
	switch event.Type {
	case "agent/ready", "user/inbox", "agent/inbox", "turn/cancel", "turn/end":
		if event.At.After(p.last) {
			p.last = event.At
		}
	}
	switch event.Type {
	case "user/inbox", "agent/inbox":
		var input struct {
			MessageID string `json:"messageId"`
		}
		if err := json.Unmarshal(event.Data, &input); err != nil {
			return fmt.Errorf("decode durable inbox event %d: %w", event.Seq, err)
		}
		if input.MessageID == "" {
			return fmt.Errorf("durable inbox event %d has no message ID", event.Seq)
		}
		if p.queued == nil {
			p.queued = make(map[string]struct{})
		}
		p.queued[input.MessageID] = struct{}{}
	case "turn/start":
		if p.openTurn {
			return fmt.Errorf("nested durable turn at event %d", event.Seq)
		}
		var source struct {
			MessageID string `json:"messageId"`
		}
		if err := json.Unmarshal(event.Data, &source); err != nil {
			return fmt.Errorf("decode durable turn source at event %d: %w", event.Seq, err)
		}
		p.openTurn = true
		p.turnID = event.Turn
		p.activeMessageID = source.MessageID
		p.enteredStep, p.cancelled = false, false
		if source.MessageID != "" {
			if _, queued := p.queued[source.MessageID]; !queued {
				return fmt.Errorf("durable turn source is not a queued inbox at event %d", event.Seq)
			}
			delete(p.queued, source.MessageID)
		}
	case "turn/cancel":
		var intent struct {
			MessageID string `json:"messageId"`
			TurnID    string `json:"turnId"`
		}
		if err := json.Unmarshal(event.Data, &intent); err != nil || intent.MessageID == "" || intent.TurnID == "" {
			return fmt.Errorf("invalid durable turn cancellation at event %d", event.Seq)
		}
		delete(p.queued, intent.MessageID)
		if p.openTurn && event.Turn == p.turnID && intent.MessageID == p.activeMessageID {
			p.cancelled = true
		}
	case "turn/end":
		if !p.openTurn || event.Turn != p.turnID {
			return fmt.Errorf("unmatched durable turn end at event %d", event.Seq)
		}
		var end struct {
			Reason string `json:"reason"`
		}
		if len(event.Data) > 0 {
			if err := json.Unmarshal(event.Data, &end); err != nil {
				return fmt.Errorf("decode durable turn end at event %d: %w", event.Seq, err)
			}
		}
		if end.Reason == TurnInterruptedBeforeStep {
			if p.activeMessageID == "" || p.enteredStep || p.cancelled {
				return fmt.Errorf("invalid durable inbox requeue at event %d", event.Seq)
			}
			p.queued[p.activeMessageID] = struct{}{}
		}
		p.openTurn = false
		p.turnID = 0
		p.activeMessageID = ""
	case "step/start":
		if p.openTurn && event.Turn == p.turnID {
			p.enteredStep = true
		}
	case "compaction/start":
		if p.openTurn {
			p.enteredStep = true
		}
	}
	return nil
}

func (p *activityProjection) snapshot(header Header, events []Event) (ActivitySnapshot, error) {
	if p.last.IsZero() {
		p.last = header.CreatedAt
	}
	if p.err != nil {
		return ActivitySnapshot{}, p.err
	}
	for p.processed < len(events) {
		if err := p.apply(events[p.processed]); err != nil {
			p.err = err
			return ActivitySnapshot{}, err
		}
		p.processed++
	}
	return ActivitySnapshot{Since: p.last, Idle: !p.openTurn && len(p.queued) == 0}, nil
}

// ProjectActivity reconstructs the projection from a durable event snapshot.
// Live Sessions use DurableActivity to incrementally extend the cached state.
func ProjectActivity(header Header, events []Event) (ActivitySnapshot, error) {
	var projection activityProjection
	return projection.snapshot(header, events)
}

func (s *Session) DurableActivity() (ActivitySnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activity.snapshot(s.header, s.events[:s.durable])
}
