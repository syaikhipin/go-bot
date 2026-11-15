package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrOpenTurnNeedsRecovery = errors.New("durable Agent turn remains open; stop and recover the Session before starting another turn")

// InboxRecord is the durable identity of one accepted input. Internal inputs
// retain their source so a Job completion cannot masquerade as human intent.
type InboxRecord struct {
	MessageID      string `json:"messageId"`
	TurnID         string `json:"turnId"`
	IdempotencyKey string `json:"idempotencyKey"`
	Content        string `json:"content"`
	SourceKind     string `json:"sourceKind,omitempty"`
	SourceID       string `json:"sourceId,omitempty"`
	GoalID         string `json:"goalId,omitempty"`
	GoalRevision   int    `json:"goalRevision,omitempty"`
	GoalRound      int    `json:"goalRound,omitempty"`
}

type InboxState struct {
	Record      InboxRecord
	StartedTurn uint64
	Cancelled   bool
	Finished    bool
}

type inboxEntry struct{ InboxState }

// inboxProjection is a replayable, incremental view of the durable prefix.
// It avoids copying the full Session history for every message admission and
// turn pickup. The JSONL log remains authoritative and is replayed once on
// cold resume, following DeepSeek's Session projection model.
type inboxProjection struct {
	processed   int
	byKey       map[string]*inboxEntry
	byInternal  map[string]*inboxEntry
	byTurn      map[string]*inboxEntry
	byMessage   map[string]*inboxEntry
	queue       []*inboxEntry
	head        int
	lastTurn    uint64
	activeTurn  uint64
	active      *inboxEntry
	enteredStep bool
	err         error
}

func (p *inboxProjection) apply(event Event) error {
	switch event.Type {
	case "user/inbox", "agent/inbox":
		var record InboxRecord
		if err := json.Unmarshal(event.Data, &record); err != nil {
			return fmt.Errorf("decode inbox event %d: %w", event.Seq, err)
		}
		if record.MessageID == "" || record.TurnID == "" || record.IdempotencyKey == "" || record.Content == "" {
			return fmt.Errorf("incomplete inbox event %d", event.Seq)
		}
		if event.Type == "user/inbox" && (record.SourceKind != "" || record.SourceID != "" || record.GoalID != "" || record.GoalRevision != 0 || record.GoalRound != 0) {
			return fmt.Errorf("human inbox has internal source at event %d", event.Seq)
		}
		if event.Type == "agent/inbox" && !validInternalSource(record) {
			return fmt.Errorf("invalid internal inbox source at event %d", event.Seq)
		}
		if p.byKey == nil {
			p.byKey = make(map[string]*inboxEntry)
			p.byInternal = make(map[string]*inboxEntry)
			p.byTurn = make(map[string]*inboxEntry)
			p.byMessage = make(map[string]*inboxEntry)
		}
		internalKey := record.SourceKind + ":" + record.SourceID
		if (event.Type == "user/inbox" && p.byKey[record.IdempotencyKey] != nil) || ((record.SourceKind == "job" || record.SourceKind == "subagent") && p.byInternal[internalKey] != nil) || p.byTurn[record.TurnID] != nil || p.byMessage[record.MessageID] != nil {
			return fmt.Errorf("duplicate inbox identity at event %d", event.Seq)
		}
		entry := &inboxEntry{InboxState: InboxState{Record: record}}
		if event.Type == "user/inbox" {
			p.byKey[record.IdempotencyKey] = entry
		} else if record.SourceKind == "job" || record.SourceKind == "subagent" {
			p.byInternal[internalKey] = entry
		}
		p.byTurn[record.TurnID] = entry
		p.byMessage[record.MessageID] = entry
		p.queue = append(p.queue, entry)
	case "turn/start":
		if event.Turn == 0 || event.Turn <= p.lastTurn || p.activeTurn != 0 {
			return fmt.Errorf("invalid turn start at event %d", event.Seq)
		}
		var source struct {
			MessageID string `json:"messageId"`
			TurnID    string `json:"turnId"`
		}
		if err := json.Unmarshal(event.Data, &source); err != nil {
			return fmt.Errorf("decode turn source at event %d: %w", event.Seq, err)
		}
		p.lastTurn, p.activeTurn = event.Turn, event.Turn
		p.enteredStep = false
		if source.MessageID != "" {
			entry := p.byMessage[source.MessageID]
			p.advanceHead()
			if entry == nil || p.head >= len(p.queue) || entry != p.queue[p.head] || entry.Record.TurnID != source.TurnID || entry.StartedTurn != 0 || entry.Cancelled {
				return fmt.Errorf("turn source does not name a queued inbox at event %d", event.Seq)
			}
			entry.StartedTurn = event.Turn
			p.active = entry
		}
	case "turn/end":
		if p.activeTurn == 0 || p.activeTurn != event.Turn {
			return fmt.Errorf("unmatched turn end at event %d", event.Seq)
		}
		var end struct {
			Reason string `json:"reason"`
		}
		if len(event.Data) > 0 {
			if err := json.Unmarshal(event.Data, &end); err != nil {
				return fmt.Errorf("decode turn end at event %d: %w", event.Seq, err)
			}
		}
		if end.Reason == TurnInterruptedBeforeStep && (p.active == nil || p.active.Cancelled || p.enteredStep) {
			return fmt.Errorf("invalid inbox requeue at event %d", event.Seq)
		}
		if p.active != nil {
			if end.Reason == TurnInterruptedBeforeStep {
				p.active.StartedTurn = 0
			} else {
				p.active.Finished = true
			}
		}
		p.activeTurn, p.active = 0, nil
	case "step/start":
		if p.activeTurn != 0 && event.Turn == p.activeTurn {
			p.enteredStep = true
		}
	case "compaction/start":
		if p.activeTurn != 0 {
			p.enteredStep = true
		}
	case "turn/cancel":
		var intent struct {
			MessageID string `json:"messageId"`
			TurnID    string `json:"turnId"`
		}
		if err := json.Unmarshal(event.Data, &intent); err != nil {
			return fmt.Errorf("decode turn cancellation at event %d: %w", event.Seq, err)
		}
		entry := p.byTurn[intent.TurnID]
		if entry == nil || entry.Record.MessageID != intent.MessageID || entry.Cancelled || entry.Finished {
			return fmt.Errorf("invalid turn cancellation at event %d", event.Seq)
		}
		entry.Cancelled = true
	}
	return nil
}

func validInternalSource(record InboxRecord) bool {
	if record.SourceID == "" {
		return false
	}
	switch record.SourceKind {
	case "job":
		return record.GoalID == "" && record.GoalRevision == 0 && record.GoalRound == 0
	case "subagent":
		return record.GoalID == "" && record.GoalRevision == 0 && record.GoalRound == 0
	case "goal":
		return record.GoalID != "" && record.GoalRevision > 0 && record.GoalRound > 0 &&
			record.SourceID == fmt.Sprintf("%s:%d:%d", record.GoalID, record.GoalRevision, record.GoalRound)
	default:
		return false
	}
}

func (p *inboxProjection) advanceHead() {
	for p.head < len(p.queue) {
		entry := p.queue[p.head]
		if entry.StartedTurn == 0 && !entry.Cancelled {
			return
		}
		p.head++
	}
}

func (s *Session) projectInbox() error {
	if s.inbox.err != nil {
		return s.inbox.err
	}
	for s.inbox.processed < s.durable {
		if err := s.inbox.apply(s.events[s.inbox.processed]); err != nil {
			s.inbox.err = err
			return err
		}
		s.inbox.processed++
	}
	return nil
}

func (s *Session) ValidateInbox() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectInbox()
}

func (s *Session) InboxByKey(key string) (InboxState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.projectInbox(); err != nil {
		return InboxState{}, false, err
	}
	entry := s.inbox.byKey[key]
	if entry == nil {
		return InboxState{}, false, nil
	}
	return entry.InboxState, true, nil
}

func (s *Session) InboxByInternalSource(kind, id string) (InboxState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.projectInbox(); err != nil {
		return InboxState{}, false, err
	}
	entry := s.inbox.byInternal[kind+":"+id]
	if entry == nil {
		return InboxState{}, false, nil
	}
	return entry.InboxState, true, nil
}

func (s *Session) InboxByTurn(turnID string) (InboxState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.projectInbox(); err != nil {
		return InboxState{}, false, err
	}
	entry := s.inbox.byTurn[turnID]
	if entry == nil {
		return InboxState{}, false, nil
	}
	return entry.InboxState, true, nil
}

// PendingGoalInboxes returns admitted-but-not-finished continuation attempts.
// Cold resume uses this to withdraw stale process-local authority before the
// new worker is made ready; ordinary human and Job messages are untouched.
func (s *Session) PendingGoalInboxes() ([]InboxState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.projectInbox(); err != nil {
		return nil, err
	}
	var pending []InboxState
	for _, entry := range s.inbox.queue {
		if entry.Record.SourceKind == "goal" && !entry.Finished && !entry.Cancelled && entry.StartedTurn == 0 {
			pending = append(pending, entry.InboxState)
		}
	}
	return pending, nil
}

// HasPendingCompetingInbox reports whether a queued goal round has a newer
// non-goal input behind it. The caller must serialize this check and its
// cancellation against admissions before claiming the round's turn.
func (s *Session) HasPendingCompetingInbox(goalMessageID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.projectInbox(); err != nil {
		return false, err
	}
	goalEntry := s.inbox.byMessage[goalMessageID]
	if goalEntry == nil || goalEntry.Record.SourceKind != "goal" || goalEntry.Cancelled || goalEntry.Finished || goalEntry.StartedTurn != 0 {
		return false, errors.New("goal message is not pending")
	}
	seenGoal := false
	for _, entry := range s.inbox.queue {
		if entry == goalEntry {
			seenGoal = true
			continue
		}
		if seenGoal && entry.StartedTurn == 0 && !entry.Cancelled && entry.Record.SourceKind != "goal" {
			return true, nil
		}
	}
	return false, nil
}

// WithdrawQueuedGoalRounds is a cold-resume fence. Goal rounds queued by a
// previous worker cannot inherit that worker's ephemeral armed permission.
func (s *Session) WithdrawQueuedGoalRounds(ctx context.Context) (int, error) {
	pending, err := s.PendingGoalInboxes()
	if err != nil {
		return 0, err
	}
	for _, round := range pending {
		if _, err := s.Append("turn/cancel", 0, 0, map[string]string{"messageId": round.Record.MessageID, "turnId": round.Record.TurnID}); err != nil {
			return 0, err
		}
	}
	if len(pending) > 0 {
		if err := s.Flush(ctx); err != nil {
			return 0, err
		}
	}
	return len(pending), nil
}

// NextInbox returns the oldest unclaimed, uncancelled durable input and the
// next logical turn number. It does not consume the input: turn/start must
// cross a flush boundary before the projection marks it as started.
func (s *Session) NextInbox() (InboxRecord, uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.projectInbox(); err != nil {
		return InboxRecord{}, 0, false, err
	}
	if s.inbox.activeTurn != 0 {
		return InboxRecord{}, 0, false, ErrOpenTurnNeedsRecovery
	}
	s.inbox.advanceHead()
	if s.inbox.head < len(s.inbox.queue) {
		return s.inbox.queue[s.inbox.head].Record, s.inbox.lastTurn + 1, true, nil
	}
	return InboxRecord{}, s.inbox.lastTurn + 1, false, nil
}

func (s *Session) NextTurn() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.projectInbox(); err != nil {
		return 0, err
	}
	if s.inbox.activeTurn != 0 {
		return 0, ErrOpenTurnNeedsRecovery
	}
	return s.inbox.lastTurn + 1, nil
}
