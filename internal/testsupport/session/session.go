package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

const SchemaVersion = 1

var ErrUncertainWrite = errors.New("session journal write outcome is uncertain; reopen and recover before continuing")

// Header is immutable session identity and assembly metadata, not a model message.
type Header struct {
	SchemaVersion int    `json:"schemaVersion"`
	SessionID     string `json:"sessionId"`
	UserID        string `json:"userId"`
	Mode          string `json:"mode"`
	Workspace     string `json:"workspace"`
	// Fork history is copied into this Session once; the final inherited
	// session/end-seed marker records the exact cut in its own event log.
	IsSeeded bool `json:"isSeeded,omitempty"`
	// A local child may have its own Session history while staying inside the
	// top-level Agent's worker and sharing its already-mounted workspace.
	ParentSessionID  string    `json:"parentSessionId,omitempty"`
	WorkspaceOwnerID string    `json:"workspaceOwnerId,omitempty"`
	DelegationDepth  int       `json:"delegationDepth,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
}

// Event is one fact in the append-only log. Data is copied on append and replay.
type Event struct {
	Seq  uint64          `json:"seq"`
	Type string          `json:"type"`
	Turn uint64          `json:"turn,omitempty"`
	Step uint64          `json:"step,omitempty"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Journal is a single-owner persistence handle. Append accepts only the next
// contiguous sequence; Flush is the durability boundary. An ambiguous write
// failure poisons the live Session until a fresh owner reopens the journal.
type Journal interface {
	Append(context.Context, []Event) error
	Flush(context.Context) error
	Close() error
}

type Session struct {
	mu        sync.Mutex
	flushMu   sync.Mutex
	header    Header
	events    []Event
	lastType  map[string]int
	durable   int
	journal   Journal
	activity  activityProjection
	inbox     inboxProjection
	usage     usageProjection
	todos     todoProjection
	goal      goalProjection
	plan      planProjection
	poison    bool
	closing   bool
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

func New(header Header, journal Journal) (*Session, error) {
	if err := validateHeader(header); err != nil {
		return nil, err
	}
	if journal == nil {
		return nil, errors.New("session journal is required")
	}
	return &Session{header: header, journal: journal, lastType: make(map[string]int)}, nil
}

func Resume(header Header, events []Event, journal Journal) (*Session, error) {
	s, err := New(header, journal)
	if err != nil {
		return nil, err
	}
	for i, event := range events {
		if event.Seq != uint64(i+1) || event.Type == "" || event.At.IsZero() {
			return nil, fmt.Errorf("invalid session event at offset %d", i)
		}
		s.events = append(s.events, cloneEvent(event))
		s.lastType[event.Type] = i
	}
	s.durable = len(s.events)
	return s, nil
}

func validateHeader(h Header) error {
	if h.SchemaVersion != SchemaVersion || h.SessionID == "" || h.UserID == "" || h.Workspace == "" || h.CreatedAt.IsZero() {
		return errors.New("invalid session header")
	}
	if h.Mode != "container" {
		return fmt.Errorf("invalid assembly mode %q", h.Mode)
	}
	if h.ParentSessionID == "" {
		if h.WorkspaceOwnerID != "" || h.DelegationDepth != 0 || h.IsSeeded {
			return errors.New("top-level Session cannot declare child lineage")
		}
	} else {
		if h.DelegationDepth < 1 || h.DelegationDepth > 16 || h.ParentSessionID == h.SessionID || h.WorkspaceOwnerID == "" || h.WorkspaceOwnerID == h.SessionID {
			return errors.New("invalid child Session lineage")
		}
		if _, err := uuid.Parse(h.ParentSessionID); err != nil {
			return errors.New("invalid parent Session ID")
		}
		if _, err := uuid.Parse(h.WorkspaceOwnerID); err != nil {
			return errors.New("invalid shared workspace owner ID")
		}
		if h.DelegationDepth == 1 && h.WorkspaceOwnerID != h.ParentSessionID {
			return errors.New("first-level child must inherit the parent workspace")
		}
	}
	return nil
}

func (h Header) EffectiveWorkspaceOwnerID() string {
	if h.WorkspaceOwnerID != "" {
		return h.WorkspaceOwnerID
	}
	return h.SessionID
}

func cloneEvent(event Event) Event {
	event.Data = append(json.RawMessage(nil), event.Data...)
	return event
}

func (s *Session) Header() Header { return s.header }

func (s *Session) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	for i := range s.events {
		out[i] = cloneEvent(s.events[i])
	}
	return out
}

// LatestEventOfType is an owner-local index over the same append-only log.
// It returns a detached event and never serves as a second persistence source.
func (s *Session) LatestEventOfType(eventType string) (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, ok := s.lastType[eventType]
	if !ok {
		return Event{}, false
	}
	return cloneEvent(s.events[index]), true
}

// EventTail returns a detached in-memory suffix after a previously observed
// sequence. It includes unflushed events for the owning Agent's internal
// projection; external consumers must use DurableEventsAfter instead.
func (s *Session) EventTail(after uint64) ([]Event, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if after > uint64(len(s.events)) {
		return nil, 0, errors.New("event tail cursor is beyond the Session")
	}
	tail := make([]Event, len(s.events)-int(after))
	for index := range tail {
		tail[index] = cloneEvent(s.events[int(after)+index])
	}
	return tail, uint64(len(s.events)), nil
}

// DurableEvents is the externally observable prefix. In-memory Append may
// lead the journal, but API/SSE consumers must never publish that suffix.
func (s *Session) DurableEvents() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, s.durable)
	for i := range out {
		out[i] = cloneEvent(s.events[i])
	}
	return out
}

// DurableEventPage returns a bounded copy from one fixed durable prefix.
// through=0 selects the current prefix on the first page; later pages pass
// back the returned through value so concurrent appends do not change it.
func (s *Session) DurableEventPage(offset, through, limit, targetBytes, maxEventBytes int) ([]Event, int, error) {
	if offset < 0 || through < 0 || limit <= 0 || targetBytes <= 0 || maxEventBytes < targetBytes {
		return nil, 0, errors.New("invalid durable event page request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if through == 0 {
		through = s.durable
	}
	if through > s.durable || offset > through {
		return nil, 0, errors.New("durable event page is outside the current prefix")
	}
	page := make([]Event, 0, min(limit, through-offset))
	used := 0
	for index := offset; index < through && len(page) < limit; index++ {
		event := s.events[index]
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, 0, err
		}
		if len(encoded) > maxEventBytes {
			return nil, 0, fmt.Errorf("durable event %d exceeds page transport limit", event.Seq)
		}
		if len(page) > 0 && used+len(encoded)+1 > targetBytes {
			break
		}
		page = append(page, cloneEvent(event))
		used += len(encoded) + 1
	}
	return page, through, nil
}

// DurableEventsAfter reads one bounded API page by event-sequence cursor.
// A cursor beyond the current durable prefix simply has no newer events.
func (s *Session) DurableEventsAfter(after uint64, limit, targetBytes, maxEventBytes int) ([]Event, error) {
	if limit <= 0 || targetBytes <= 0 || maxEventBytes < targetBytes {
		return nil, errors.New("invalid durable event cursor request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if after >= uint64(s.durable) {
		return []Event{}, nil
	}
	page := make([]Event, 0, min(limit, s.durable-int(after)))
	used := 0
	for index := int(after); index < s.durable && len(page) < limit; index++ {
		event := s.events[index]
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		if len(encoded) > maxEventBytes {
			return nil, fmt.Errorf("durable event %d exceeds transport limit", event.Seq)
		}
		if len(page) > 0 && used+len(encoded)+1 > targetBytes {
			break
		}
		page = append(page, cloneEvent(event))
		used += len(encoded) + 1
	}
	return page, nil
}

// Append updates the live in-memory log. API acknowledgements and external
// side effects must call Flush and wait for it to succeed first.
func (s *Session) Append(eventType string, turn, step uint64, data any) (Event, error) {
	if eventType == "" {
		return Event{}, errors.New("event type is required")
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("encode session event: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || s.poison {
		return Event{}, ErrUncertainWrite
	}
	event := Event{Seq: uint64(len(s.events) + 1), Type: eventType, Turn: turn, Step: step, At: time.Now().UTC(), Data: encoded}
	// Reject an event that no journal can later read before it enters the
	// in-memory prefix. Otherwise Flush would poison the Session for a known,
	// deterministic size error rather than allowing a bounded error result.
	if _, err := encodeStoredLine(storedLine{Kind: "event", Event: &event}); err != nil {
		return Event{}, err
	}
	s.events = append(s.events, event)
	s.lastType[eventType] = len(s.events) - 1
	return cloneEvent(event), nil
}

// Flush persists the complete new prefix. A failed Append or Flush may have
// reached storage; this Session stops rather than blindly replaying the batch.
func (s *Session) Flush(ctx context.Context) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	return s.flushLocked(ctx)
}

// flushLocked serializes the journal while leaving the in-memory projection
// readable during a slow OSS call. Concurrent Append may extend the tail;
// only the prefix captured here becomes durable on successful Flush.
func (s *Session) flushLocked(ctx context.Context) error {
	s.mu.Lock()
	if s.closed || s.poison {
		s.mu.Unlock()
		return ErrUncertainWrite
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	through := len(s.events)
	batch := make([]Event, through-s.durable)
	copy(batch, s.events[s.durable:through])
	s.mu.Unlock()
	if len(batch) > 0 {
		if err := s.journal.Append(ctx, batch); err != nil {
			s.mu.Lock()
			s.poison = true
			s.mu.Unlock()
			return fmt.Errorf("append session events: %w: %w", err, ErrUncertainWrite)
		}
	}
	if err := s.journal.Flush(ctx); err != nil {
		s.mu.Lock()
		s.poison = true
		s.mu.Unlock()
		return fmt.Errorf("flush session events: %w: %w", err, ErrUncertainWrite)
	}
	s.mu.Lock()
	s.durable = through
	s.mu.Unlock()
	return nil
}

func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		s.flushMu.Lock()
		defer s.flushMu.Unlock()
		// DeepSeek's write handle drains on close. Normal shutdown must not
		// drop a buffered event without an explicit earlier checkpoint.
		flushErr := s.flushLocked(context.Background())
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.closeErr = errors.Join(flushErr, s.journal.Close())
	})
	return s.closeErr
}
