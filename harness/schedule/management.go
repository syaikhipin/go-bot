package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound         = errors.New("schedule_not_found")
	ErrConflict         = errors.New("schedule_conflict")
	ErrExecutionPending = errors.New("schedule_execution_pending")
	ErrUnavailable      = errors.New("schedule_unavailable")
	ErrInvalid          = errors.New("schedule_input_invalid")
)

type Timing struct {
	Kind         string          `json:"kind"`
	AfterSeconds *int64          `json:"after_seconds,omitempty"`
	At           json.RawMessage `json:"at,omitempty"`
	EverySeconds *int64          `json:"every_seconds,omitempty"`
	Daily        *LocalTimeInput `json:"daily,omitempty"`
	Weekly       *LocalTimeInput `json:"weekly,omitempty"`
	Cron         *CronInput      `json:"cron,omitempty"`
	RRule        *string         `json:"rrule,omitempty"`
}

type CreateRequest struct {
	Title  string  `json:"title"`
	Prompt string  `json:"prompt"`
	Timing Timing  `json:"timing"`
	Target *Target `json:"target,omitempty"`
}

type Patch struct {
	Title  *string `json:"title,omitempty"`
	Prompt *string `json:"prompt,omitempty"`
	Timing *Timing `json:"timing,omitempty"`
	Target *Target `json:"target,omitempty"`
	Active *bool   `json:"active,omitempty"`
}

type EditRequest struct {
	ID                string          `json:"id"`
	Action            string          `json:"action"`
	Patch             *Patch          `json:"patch,omitempty"`
	ExpectedUpdatedAt json.RawMessage `json:"expectedUpdatedAt,omitempty"`
}

type Entry struct {
	Record              Record   `json:"record"`
	Target              *Target  `json:"target,omitempty"`
	Status              string   `json:"status"`
	SessionID           string   `json:"sessionId,omitempty"`
	InvolvedSessionIDs  []string `json:"involvedSessionIds,omitempty"`
	CreatedAt           string   `json:"createdAt,omitempty"`
	UpdatedAt           string   `json:"updatedAt,omitempty"`
	LastDelivery        *Receipt `json:"lastDelivery,omitempty"`
	ScheduledAtLocal    string   `json:"scheduledAtLocal"`
	ExecutionActive     bool     `json:"executionActive,omitempty"`
	DeliveryFloorEndsAt string   `json:"deliveryFloorEndsAt,omitempty"`
}

type ListPage struct {
	Items  []Entry `json:"items"`
	Shown  int     `json:"shown"`
	Total  int     `json:"total"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
}

func isEnded(status string) bool { return status == "ended" || status == "inactive" }

func nextRevision(previous string, now time.Time) (string, error) {
	if last, err := time.Parse(time.RFC3339Nano, previous); err == nil && !now.Truncate(time.Millisecond).After(last) {
		now = last.Add(time.Millisecond)
	}
	return canonicalUTC(now)
}

func (s *Service) entryLocked(task Task) Entry {
	task = cloneTask(task)
	status := task.Status
	if isEnded(status) {
		status = "ended"
	}
	sessionID := task.deliverySession()
	if task.Target != nil && task.Target.Kind != "session" {
		sessionID = ""
	}
	entry := Entry{Record: task.Record, Target: task.Target, Status: status, SessionID: sessionID,
		InvolvedSessionIDs: task.InvolvedSessionIDs, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
		LastDelivery: task.LastDelivery, ScheduledAtLocal: localScheduledAt(task.Record)}
	if _, held := s.executing[task.Record.ID]; held {
		entry.ExecutionActive = true
	}
	if floor := s.nextAdmissionAt(task); floor.After(s.now()) {
		entry.DeliveryFloorEndsAt, _ = canonicalUTC(floor)
	}
	return entry
}

func localScheduledAt(record Record) string {
	at, err := time.Parse(time.RFC3339Nano, record.ScheduledAt)
	if err != nil {
		return record.ScheduledAt
	}
	zone, _, _ := parseZone(record.TimeZone)
	if record.Kind == "rrule" {
		if rule, err := parseRRule(record.RRule, false); err == nil {
			zone = rule.location
		}
	}
	if zone == nil {
		zone = time.UTC
	}
	return at.In(zone).Format("2006-01-02T15:04:05-07:00")
}

func (s *Service) Catalog() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || s.poisoned {
		return nil, ErrUnavailable
	}
	entries := make([]Entry, 0, len(s.order))
	for _, id := range s.order {
		entries = append(entries, s.entryLocked(s.tasks[id]))
	}
	sort.Slice(entries, func(i, j int) bool {
		left, right := entries[i], entries[j]
		if (left.Status == "active") != (right.Status == "active") {
			return left.Status == "active"
		}
		if left.Status == "active" {
			if left.Record.ScheduledAt != right.Record.ScheduledAt {
				return left.Record.ScheduledAt < right.Record.ScheduledAt
			}
		} else if l, r := recentTaskAt(left), recentTaskAt(right); l != r {
			return l > r
		}
		return left.Record.ID < right.Record.ID
	})
	return entries, nil
}

func recentTaskAt(entry Entry) string {
	if entry.LastDelivery != nil {
		return entry.LastDelivery.DeliveredAt
	}
	if entry.UpdatedAt != "" {
		return entry.UpdatedAt
	}
	if entry.CreatedAt != "" {
		return entry.CreatedAt
	}
	return entry.Record.ScheduledAt
}

func validTargetRequest(target *Target) bool {
	return target == nil || (target.Kind == "session" || target.SessionID == "") && validateTarget(*target) == nil
}

func (s *Service) ListTasks(active *bool, limit, offset int) (ListPage, error) {
	if limit < 1 || limit > 500 || offset < 0 {
		return ListPage{}, ErrInvalid
	}
	entries, err := s.Catalog()
	if err != nil {
		return ListPage{}, err
	}
	matching := entries[:0]
	for _, entry := range entries {
		if active == nil || (entry.Status == "active") == *active {
			matching = append(matching, entry)
		}
	}
	start := min(offset, len(matching))
	page := matching[start : start+min(limit, len(matching)-start)]
	return ListPage{Items: page, Shown: len(page), Total: len(matching), Limit: limit, Offset: offset}, nil
}

func (s *Service) ViewTask(id string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || s.poisoned {
		return Entry{}, ErrUnavailable
	}
	task, ok := s.tasks[id]
	if !ok {
		return Entry{}, ErrNotFound
	}
	return s.entryLocked(task), nil
}

func (s *Service) History(id string, limit int, before *string) (HistoryPage, error) {
	return s.historyFor("", id, limit, before, true)
}

func (s *Service) CreateTask(ctx context.Context, actor string, request CreateRequest) (Entry, error) {
	if !validTargetRequest(request.Target) {
		return Entry{}, ErrInvalid
	}
	task, err := s.create(ctx, actor, request.Target, func(now time.Time) (Record, error) {
		record, err := request.Timing.build(uuid.NewString(), request.Title, request.Prompt, now)
		if err != nil {
			return Record{}, errors.Join(ErrInvalid, err)
		}
		return record, nil
	}, true)
	if err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.tasks[task.Record.ID]; ok {
		task = current
	}
	return s.entryLocked(task), nil
}

func (s *Service) EditTask(ctx context.Context, actor string, request EditRequest) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || s.poisoned {
		return Entry{}, ErrUnavailable
	}
	task, ok := s.tasks[request.ID]
	if !ok {
		return Entry{}, ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	if request.ExpectedUpdatedAt != nil {
		var expected string
		if err := json.Unmarshal(request.ExpectedUpdatedAt, &expected); err != nil {
			return Entry{}, ErrInvalid
		}
		if expected != task.UpdatedAt {
			return Entry{}, ErrConflict
		}
	}
	patch := request.Patch
	if request.Action == "delete" && patch == nil {
		if err := s.mutateLocked(ctx, request.ID, nil, true); err != nil {
			return Entry{}, err
		}
		delete(s.executing, request.ID)
		s.queueNoticeLocked(task, actor, "deleted")
		return Entry{}, nil
	}
	if request.Action != "update" || patch == nil || *patch == (Patch{}) || !validTargetRequest(patch.Target) {
		return Entry{}, ErrInvalid
	}
	action := ""
	if patch.Active != nil {
		action = "pause"
		if *patch.Active {
			action = "resume"
		}
	}
	updated, err := s.editLocked(ctx, task, actor, action, patch.Target, func(previous Record, now time.Time) (Record, error) {
		record, err := applyTimingPatch(previous, *patch, now)
		if err != nil {
			return Record{}, errors.Join(ErrInvalid, err)
		}
		return record, nil
	}, true)
	if err != nil {
		return Entry{}, err
	}
	return s.entryLocked(updated), nil
}

func (s *Service) DeleteInactive(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || s.poisoned {
		return 0, ErrUnavailable
	}
	next := make(map[string]Task, len(s.tasks))
	order := make([]string, 0, len(s.order))
	var removed []string
	for _, id := range s.order {
		task := s.tasks[id]
		if isEnded(task.Status) {
			removed = append(removed, id)
		} else {
			next[id] = task
			order = append(order, id)
		}
	}
	if len(removed) == 0 {
		return 0, nil
	}
	if err := s.saveLocked(ctx, next, order, false); err != nil {
		return 0, err
	}
	for _, id := range removed {
		delete(s.executing, id)
	}
	return len(removed), nil
}

func recordTiming(record Record) Record {
	record.ID, record.Title, record.Prompt = "", "", ""
	if record.Kind != "at" && record.Kind != "after" {
		record.ScheduledAt = ""
	}
	return record
}

func applyTimingPatch(previous Record, patch Patch, now time.Time) (Record, error) {
	title, prompt := previous.Title, previous.Prompt
	if patch.Title != nil {
		title = *patch.Title
	}
	if patch.Prompt != nil {
		prompt = *patch.Prompt
	}
	text, err := newRecord(previous.ID, title, prompt, previous.Kind)
	if err != nil {
		return Record{}, err
	}
	previous.Title, previous.Prompt = text.Title, text.Prompt
	resume := patch.Active != nil && *patch.Active
	if patch.Timing != nil {
		equal, err := patch.Timing.equivalent(previous)
		if err != nil {
			return Record{}, err
		}
		if !equal {
			return patch.Timing.build(previous.ID, text.Title, text.Prompt, now)
		}
	}
	if !resume {
		return previous, nil
	}
	switch previous.Kind {
	case "after", "at":
		if taskDueAt(Task{Record: previous}, now) {
			return Record{}, errors.New("Set a future time before resuming this task")
		}
	case "every":
		return NewEvery(previous.ID, text.Title, text.Prompt, previous.EverySeconds, now)
	case "daily":
		return NewDaily(previous.ID, text.Title, text.Prompt, LocalTimeInput{Time: previous.Time, TimeZone: previous.TimeZone}, now)
	case "weekly":
		return NewWeekly(previous.ID, text.Title, text.Prompt, LocalTimeInput{Time: previous.Time, TimeZone: previous.TimeZone, Weekdays: previous.Weekdays}, now)
	case "cron":
		return NewCron(previous.ID, text.Title, text.Prompt, CronInput{Expression: previous.Expression, TimeZone: previous.TimeZone}, now)
	case "rrule":
		rule, err := parseRRule(previous.RRule, false)
		if err != nil {
			return Record{}, err
		}
		_, next, err := rule.window(now, now)
		if err != nil {
			return Record{}, err
		}
		if next.IsZero() {
			return Record{}, errors.New("RRULE has no future occurrence")
		}
		previous.ScheduledAt, err = canonicalUTC(next)
		return previous, err
	}
	return previous, nil
}

func (t Timing) valid() bool {
	fields := map[string]bool{"after": t.AfterSeconds != nil, "at": len(t.At) != 0, "every": t.EverySeconds != nil,
		"daily": t.Daily != nil, "weekly": t.Weekly != nil, "cron": t.Cron != nil, "rrule": t.RRule != nil}
	for kind, present := range fields {
		if present != (kind == t.Kind) {
			return false
		}
	}
	return fields[t.Kind]
}

func (t Timing) at() (string, error) {
	var instant string
	if len(t.At) > 0 && t.At[0] == '"' {
		if err := json.Unmarshal(t.At, &instant); err != nil {
			return "", err
		}
		return NormalizeAt(instant)
	}
	var local LocalAtInput
	decoder := json.NewDecoder(bytes.NewReader(t.At))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&local); err != nil {
		return "", err
	}
	return NormalizeLocalAt(local)
}

func (t Timing) build(id, title, prompt string, now time.Time) (Record, error) {
	if !t.valid() {
		return Record{}, errors.New("Supply the selector matching the schedule timing kind")
	}
	switch t.Kind {
	case "after":
		return NewAfter(id, title, prompt, *t.AfterSeconds, now)
	case "at":
		at, err := t.at()
		if err != nil {
			return Record{}, err
		}
		return NewAt(id, title, prompt, at, now)
	case "every":
		return NewEvery(id, title, prompt, *t.EverySeconds, now)
	case "daily":
		return NewDaily(id, title, prompt, *t.Daily, now)
	case "weekly":
		return NewWeekly(id, title, prompt, *t.Weekly, now)
	case "cron":
		return NewCron(id, title, prompt, *t.Cron, now)
	default:
		return NewRRule(id, title, prompt, *t.RRule, now)
	}
}

func (t Timing) equivalent(record Record) (bool, error) {
	if !t.valid() {
		return false, errors.New("Supply the selector matching the schedule timing kind")
	}
	switch {
	case t.Kind == "at" && (record.Kind == "after" || record.Kind == "at"):
		at, err := t.at()
		return at == record.ScheduledAt, err
	case record.Kind != t.Kind:
		return false, nil
	case t.Kind == "every":
		return *t.EverySeconds == record.EverySeconds, nil
	case t.Kind == "daily":
		return EquivalentDailyRule(record, *t.Daily)
	case t.Kind == "weekly":
		return EquivalentWeeklyRule(record, *t.Weekly)
	case t.Kind == "cron":
		return EquivalentCronRule(record, *t.Cron)
	case t.Kind == "rrule":
		return EquivalentRRule(record, strings.TrimSpace(*t.RRule))
	default:
		return false, nil
	}
}
