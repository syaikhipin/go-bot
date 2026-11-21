package proactive

import (
	"time"
)

const (
	StabilityWindow = 10 * time.Minute
	MinimumInterval = 30 * time.Minute
)

type WakeReason string

const (
	InitialWake WakeReason = "initial"
	TimedWake   WakeReason = "scheduled-check"
	ChangeWake  WakeReason = "information-changed"
	RecoverWake WakeReason = "recovery"
	EnabledWake WakeReason = "enabled"
)

// State is a recovery projection, never a second copy of conversation data.
// NextCheck is absent only before a successful first run; every accepted
// Result has a non-null nextCheck. Error does not mean the AI cancelled it.
type State struct {
	SessionID        string `json:"sessionId"`
	MainSessionID    string `json:"mainSessionId"`
	NextCheck        *Check `json:"nextCheck,omitempty"`
	LastRunStartedAt int64  `json:"lastRunStartedAt,omitempty"`
	LastCompletedAt  int64  `json:"lastCompletedAt,omitempty"`
	PendingChange    bool   `json:"pendingChange"`
	LastChangeAt     int64  `json:"lastChangeAt,omitempty"`
	LastError        string `json:"lastError,omitempty"`
	// ResultTurn identifies the successful structured result already projected.
	ResultTurn uint64 `json:"resultTurn,omitempty"`
	// A durable notification uses this same message ID when retried. Its body
	// remains in the background journal, not in a second change-content queue.
	DeliveryMessageID string `json:"deliveryMessageId,omitempty"`
	DeliveryTurn      uint64 `json:"deliveryTurn,omitempty"`
}

func (s *State) Changed(now time.Time) {
	s.PendingChange = true
	s.LastChangeAt = now.UnixMilli()
}

// ChangeEligibleAt applies only to the information-change trigger. A timed
// check, initialization or recovery does not pass through this gate.
func (s State) ChangeEligibleAt() time.Time {
	if !s.PendingChange {
		return time.Time{}
	}
	at := time.UnixMilli(s.LastChangeAt).Add(StabilityWindow)
	if s.LastRunStartedAt != 0 {
		minimum := time.UnixMilli(s.LastRunStartedAt).Add(MinimumInterval)
		if minimum.After(at) {
			at = minimum
		}
	}
	return at
}

func (s State) NextWake() (time.Time, WakeReason) {
	var at time.Time
	var reason WakeReason
	// A failed earlier change check does not cancel an AI-selected future
	// opportunity. Suppress only a timer already attempted at/after its time,
	// so a failed overdue check cannot spin on the same due timestamp.
	if s.NextCheck != nil && (s.LastError == "" || s.NextCheck.UnixMilli() > s.LastRunStartedAt) {
		at, reason = time.UnixMilli(s.NextCheck.UnixMilli()), TimedWake
	}
	if changed := s.ChangeEligibleAt(); !changed.IsZero() && (s.LastError == "" || s.LastChangeAt > s.LastRunStartedAt) && (at.IsZero() || changed.Before(at)) {
		at, reason = changed, ChangeWake
	}
	return at, reason
}
