package sessionformat

import (
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// Preparation owns one validated, unpublished Go Session. It has no writer or
// registry entry until Attach. Failed validation therefore creates no disk data.
type Preparation struct {
	mu       sync.Mutex
	session  *LiveSession
	attached bool
}

// PrepareSession snapshots borrowed seed data and applies the upstream fresh
// constructor marker rules. Nil seed means omitted; a non-nil empty seed is an
// explicit replay seed and receives an end-seed marker. It does not repair an
// interrupted turn: only resume owns that operation.
func PrepareSession(header Header, seed []Event, inherited *uint64) (*Preparation, error) {
	return prepareSessionAt(header, seed, inherited, time.Now().UnixMilli())
}

func prepareSessionAt(header Header, seed []Event, inherited *uint64, timestamp int64) (*Preparation, error) {
	if err := header.Validate(); err != nil {
		return nil, err
	}
	if header.IsSeeded && (seed == nil || inherited == nil) {
		return nil, errors.New("seeded Session requires an explicit seed and inherited event count")
	}
	count := uint64(0)
	if inherited != nil {
		count = *inherited
	}
	if count > uint64(len(seed)) || count > uint64(maxSafeInteger) || (!header.IsSeeded && count != 0) {
		return nil, errors.New("invalid Session inherited event count")
	}
	events := cloneV4Events(seed)
	marked := false
	if count < uint64(len(events)) && events[count].Type == "session/end-seed" {
		var data struct {
			Inherited bool `json:"inherited"`
		}
		if json.Unmarshal(events[count].Data, &data) == nil {
			marked = data.Inherited
		}
	}
	if header.IsSeeded && count != uint64(len(events)) && !marked {
		return nil, errors.New("seed must equal the inherited prefix or mark its inherited cut")
	}
	if marked {
		for _, event := range events[count+1:] {
			if event.Type != "session/end-seed" {
				continue
			}
			var data struct {
				Inherited bool `json:"inherited"`
			}
			if json.Unmarshal(event.Data, &data) == nil && data.Inherited {
				return nil, errors.New("inherited cut must identify the final inherited marker")
			}
		}
	}
	if seed != nil {
		if header.IsSeeded && !marked {
			events = append(events, Event{Type: "session/end-seed", Seq: uint64(len(events)), Time: timestamp, Data: json.RawMessage(`{"inherited":true}`)})
		} else if !header.IsSeeded && (len(events) == 0 || events[len(events)-1].Type != "session/end-seed") {
			events = append(events, Event{Type: "session/end-seed", Seq: uint64(len(events)), Time: timestamp, Data: json.RawMessage(`{}`)})
		}
	}
	s, err := replaySession(PhysicalArtifact{Header: header, Events: events, InheritedEventCount: count}, nil, 0)
	if err != nil {
		return nil, err
	}
	return &Preparation{session: s}, nil
}

func (p *Preparation) Header() Header  { return p.session.Header() }
func (p *Preparation) Events() []Event { return p.session.Events() }

// Attach transfers the exact prepared Session to its single real write handle.
// storedCount identifies the prefix already atomically persisted by storage;
// any remaining constructor events still need the caller's checkpoint.
func (p *Preparation) Attach(journal EventJournal, storedCount int) (*LiveSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if journal == nil || p.attached || storedCount < 0 || storedCount > len(p.session.events) {
		return nil, errors.New("Session preparation already attached or writer missing")
	}
	p.attached = true
	p.session.journal = journal
	p.session.durable = storedCount
	return p.session, nil
}
