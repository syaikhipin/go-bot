package sessionformat

import "time"

// ActivitySnapshot is worker telemetry for session activity. It is derived
// from the durable v4 Session, not written as another Session event.
type ActivitySnapshot struct {
	Since time.Time `json:"since"`
	Idle  bool      `json:"idle"`
}

// DurableActivity leaves a Session busy while its inbox has work or a turn
// has no durable end. The worker also checks its in-flight driver separately.
func (s *LiveSession) DurableActivity() ActivitySnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := ActivitySnapshot{Since: time.UnixMilli(s.header.CreatedAt).UTC(), Idle: true}
	openTurn := false
	for _, event := range s.events[:s.durable] {
		at := time.UnixMilli(event.Time).UTC()
		if at.After(snapshot.Since) {
			snapshot.Since = at
		}
		switch event.Type {
		case "turn/start":
			openTurn = true
		case "turn/end":
			openTurn = false
		}
	}
	snapshot.Idle = !openTurn && len(s.inbox.NextTurn) == 0 && len(s.inbox.NextStep) == 0
	return snapshot
}
