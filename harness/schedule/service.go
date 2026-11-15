package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Store persists the Schedule domain independently of v4 Session logs.
// One user Host owns the shared task table and serializes its writes.
type Store interface {
	Load(context.Context) (Document, error)
	Save(context.Context, Document) error
}

type Document struct {
	Version int    `json:"version"`
	Tasks   []Task `json:"tasks"`
}

type Receipt struct {
	ScheduledAt     string `json:"scheduledAt"`
	DeliveredAt     string `json:"deliveredAt"`
	MessageID       string `json:"messageId"`
	TargetSessionID string `json:"targetSessionId,omitempty"`
	Prompt          string `json:"prompt,omitempty"`
}

type History struct {
	Records                   []Receipt `json:"records"`
	EarlierRecordsUnavailable bool      `json:"earlierRecordsUnavailable"`
	EarlierRecordsPruned      bool      `json:"earlierRecordsPruned,omitempty"`
}

const deliveryHistoryDays = 30
const deliveryHistoryRecords = 200

type HistoryPage struct {
	ID                        string    `json:"id"`
	Records                   []Receipt `json:"records"`
	EarlierRecordsUnavailable bool      `json:"earlierRecordsUnavailable"`
	EarlierRecordsPruned      bool      `json:"earlierRecordsPruned"`
	Retention                 struct {
		Days    int `json:"days"`
		Records int `json:"records"`
	} `json:"retention"`
	NextBefore string `json:"nextBefore,omitempty"`
	Code       string `json:"code,omitempty"`
}

type Task struct {
	Target             *Target          `json:"target,omitempty"`
	InvolvedSessionIDs []string         `json:"involvedSessionIds,omitempty"`
	PendingDelivery    *PendingDelivery `json:"pendingDelivery,omitempty"`
	CreatedAt          string           `json:"createdAt,omitempty"`
	UpdatedAt          string           `json:"updatedAt,omitempty"`
	SessionID          string           `json:"sessionId,omitempty"`
	Record             Record           `json:"record"`
	Status             string           `json:"status"`
	LastDelivery       *Receipt         `json:"lastDelivery,omitempty"`
	DeliveryHistory    History          `json:"deliveryHistory"`
}

type Deliver func(context.Context, string) (messageID string, err error)
type HostDeliver func(context.Context, string, string) (messageID string, err error)
type HostPrepare func(context.Context, string) error
type RuleBuilder func(time.Time) (Record, error)

type Service struct {
	store              Store
	deliver            HostDeliver
	prepare            HostPrepare
	createSession      HostPrepare
	createDestination  HostCreateDestination
	checkSessionTarget HostPrepare
	observeExecution   HostExecution
	notifyChange       HostNotice
	notices            chan struct{}
	noticeQueue        []changeNotice
	noticesDone        chan struct{}
	executing          map[string]Receipt
	limits             Limits
	hostWide           bool
	now                func() time.Time
	mu                 sync.Mutex
	tasks              map[string]Task
	order              []string
	wake               chan struct{}
	done               chan struct{}
	changed            chan struct{}
	cancelMu           sync.Mutex
	cancel             context.CancelFunc
	closing            atomic.Bool
	started            bool
	closed             bool
	poisoned           bool
}

func NewService(store Store, deliver Deliver) (*Service, error) {
	if store == nil || deliver == nil {
		return nil, errors.New("Schedule requires storage and delivery")
	}
	return &Service{store: store, deliver: func(ctx context.Context, _ string, text string) (string, error) {
		return deliver(ctx, text)
	}, now: time.Now, tasks: make(map[string]Task), wake: make(chan struct{}, 1)}, nil
}

// NewHostService owns one task table and timer for every Session of a user.
// Each task persists its original Session ID so due delivery can cold-resume
// that Agent without relying on the Session that happened to create the task.
func NewHostService(store Store, prepare HostPrepare, deliver HostDeliver) (*Service, error) {
	if store == nil || prepare == nil || deliver == nil {
		return nil, errors.New("Host Schedule requires storage, Session activation, and delivery")
	}
	return &Service{store: store, prepare: prepare, deliver: deliver, hostWide: true, now: time.Now, tasks: make(map[string]Task), wake: make(chan struct{}, 1)}, nil
}

// SetSessionCreator configures the embedding's idempotent restore-or-create
// operation. The Host supplies it before starting the Schedule service.
func (s *Service) SetSessionCreator(create HostPrepare) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createSession = create
}

func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed || s.closing.Load() {
		return errors.New("Schedule service already started or closed")
	}
	document, err := s.store.Load(ctx)
	if err != nil {
		return err
	}
	if document.Version != 1 {
		return fmt.Errorf("unsupported Schedule document version %d", document.Version)
	}
	if err := validateDocument(document); err != nil {
		return err
	}
	tasks := make(map[string]Task, len(document.Tasks))
	order := make([]string, 0, len(document.Tasks))
	for _, task := range document.Tasks {
		if s.hostWide && task.SessionID == "" && task.Target == nil {
			return errors.New("Host Schedule task is missing its Session ID")
		}
		if task.Record.ID == "" || !taskStatusValid(task.Status) || tasks[task.Record.ID].Record.ID != "" {
			return errors.New("Schedule storage contains an invalid or duplicate task")
		}
		if _, err := time.Parse("2006-01-02T15:04:05.000Z", task.Record.ScheduledAt); err != nil {
			return err
		}
		tasks[task.Record.ID] = task
		order = append(order, task.Record.ID)
	}
	s.tasks = tasks
	s.order = order
	s.executing = make(map[string]Receipt)
	s.limits = s.limits.withDefaults()
	s.started = true
	s.done = make(chan struct{})
	s.changed = make(chan struct{})
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancelMu.Lock()
	s.cancel = cancel
	if s.closing.Load() {
		cancel()
	}
	s.cancelMu.Unlock()
	if s.notifyChange != nil {
		s.notices = make(chan struct{}, 1)
		s.noticesDone = make(chan struct{})
		go s.deliverNotices(runCtx)
	}
	go s.run(runCtx)
	return nil
}

func (s *Service) Close() {
	// Delivery holds mu to serialize its receipt with task management. Cancel
	// before waiting for mu so a delivery blocked on ctx.Done can finish.
	s.closing.Store(true)
	s.cancelMu.Lock()
	cancel := s.cancel
	s.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
	}
	done := s.done
	noticesDone := s.noticesDone
	s.mu.Unlock()
	if done != nil {
		<-done
	}
	if noticesDone != nil {
		<-noticesDone
	}
}

func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// NotifyModelChange retries tasks that were waiting for a usable model.
func (s *Service) NotifyModelChange() {
	s.signal()
}

func cloneTask(task Task) Task {
	task.InvolvedSessionIDs = append([]string(nil), task.InvolvedSessionIDs...)
	if task.Target != nil {
		target := *task.Target
		if target.Create != nil {
			create := *target.Create
			target.Create = &create
		}
		task.Target = &target
	}
	if task.PendingDelivery != nil {
		pending := *task.PendingDelivery
		task.PendingDelivery = &pending
	}
	if task.Record.Weekdays != nil {
		task.Record.Weekdays = append([]int(nil), task.Record.Weekdays...)
	}
	if task.LastDelivery != nil {
		receipt := *task.LastDelivery
		task.LastDelivery = &receipt
	}
	if task.DeliveryHistory.Records != nil {
		task.DeliveryHistory.Records = append([]Receipt{}, task.DeliveryHistory.Records...)
	}
	return task
}

func snapshotTasks(tasks map[string]Task, order []string) Document {
	document := Document{Version: 1, Tasks: make([]Task, 0, len(order))}
	for _, id := range order {
		document.Tasks = append(document.Tasks, cloneTask(tasks[id]))
	}
	return document
}

func (s *Service) saveLocked(ctx context.Context, next map[string]Task, nextOrder []string, notify bool) error {
	// Rule construction and update callbacks run after the caller's first
	// cancellation check. If cancellation arrived there, no persistence has
	// started and the Host Schedule must remain usable for later requests.
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(next) != len(nextOrder) {
		return errors.New("Schedule task order is inconsistent")
	}
	document := snapshotTasks(next, nextOrder)
	if err := validateDocument(document); err != nil {
		return err
	}
	if err := s.store.Save(ctx, document); err != nil {
		s.poisoned = true
		s.publishChangeLocked()
		return err
	}
	s.tasks = next
	s.order = nextOrder
	s.publishChangeLocked()
	if notify {
		s.signal()
	}
	return nil
}

func (s *Service) mutateLocked(ctx context.Context, id string, replacement *Task, notify bool) error {
	next := make(map[string]Task, len(s.tasks)+1)
	for key, task := range s.tasks {
		next[key] = task
	}
	nextOrder := append([]string(nil), s.order...)
	if replacement == nil {
		delete(next, id)
		for index, key := range nextOrder {
			if key == id {
				nextOrder = append(nextOrder[:index], nextOrder[index+1:]...)
				break
			}
		}
	} else {
		if _, exists := next[id]; !exists {
			nextOrder = append(nextOrder, id)
		}
		next[id] = cloneTask(*replacement)
	}
	return s.saveLocked(ctx, next, nextOrder, notify)
}

func (s *Service) Create(ctx context.Context, build RuleBuilder) (Task, error) {
	return s.CreateFor(ctx, "", build)
}

func (s *Service) CreateFor(ctx context.Context, sessionID string, build RuleBuilder) (Task, error) {
	return s.CreateForTarget(ctx, sessionID, nil, build)
}

func (s *Service) CreateForTarget(ctx context.Context, sessionID string, target *Target, build RuleBuilder) (Task, error) {
	return s.create(ctx, sessionID, target, build, false)
}

func (s *Service) create(ctx context.Context, sessionID string, target *Target, build RuleBuilder, hostManagement bool) (Task, error) {
	if build == nil {
		return Task{}, errors.New("Schedule rule builder is required")
	}
	if s.hostWide && sessionID == "" && !hostManagement {
		return Task{}, errors.New("Host Schedule requires a Session ID")
	}
	// DeepSeek anchors the rule to the request time before joining Schedule's
	// serialized write queue. A slow delivery or earlier write must not shift
	// an after_seconds/every_seconds reminder into the future.
	record, err := build(s.now().UTC())
	if err != nil {
		return Task{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || s.poisoned {
		return Task{}, errors.New("Schedule service is not writable")
	}
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	if record.ID == "" || s.tasks[record.ID].Record.ID != "" {
		return Task{}, errors.New("Schedule ID is empty or already exists")
	}
	task := Task{SessionID: sessionID, Record: record, Status: "active", DeliveryHistory: History{Records: []Receipt{}}}
	if s.hostWide {
		when, err := canonicalUTC(s.now())
		if err != nil {
			return Task{}, err
		}
		task.CreatedAt, task.UpdatedAt = when, when
		if sessionID != "" {
			task.InvolvedSessionIDs = []string{sessionID}
		}
		if target == nil {
			if hostManagement {
				target = &Target{Kind: "new_once"}
			} else {
				target = &Target{Kind: "session", SessionID: sessionID}
			}
		}
		task.Target, err = s.configureTarget(ctx, *target, nil)
		if err != nil {
			return Task{}, err
		}
		if task.Target.Kind != "new" && task.Target.SessionID != sessionID {
			task.InvolvedSessionIDs = append(task.InvolvedSessionIDs, task.Target.SessionID)
		}
	}
	if err := s.mutateLocked(ctx, record.ID, &task, true); err != nil {
		return Task{}, err
	}
	s.queueNoticeLocked(task, sessionID, "created")
	return cloneTask(task), nil
}

func (s *Service) List() ([]Task, error) {
	return s.ListFor("")
}

func (s *Service) ListFor(sessionID string) ([]Task, error) {
	if s.hostWide && sessionID == "" {
		return nil, errors.New("Host Schedule requires a Session ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || s.poisoned {
		return nil, errors.New("Schedule service is not readable; reopen its storage")
	}
	var tasks []Task
	for _, id := range s.order {
		task := s.tasks[id]
		if task.Status != "inactive" && task.Status != "ended" && (!s.hostWide || task.visibleTo(sessionID)) {
			tasks = append(tasks, cloneTask(task))
		}
	}
	return tasks, nil
}

func (s *Service) Lookup(id string) (Task, bool, error) {
	return s.LookupFor("", id)
}

func (s *Service) LookupFor(sessionID, id string) (Task, bool, error) {
	if s.hostWide && sessionID == "" {
		return Task{}, false, errors.New("Host Schedule requires a Session ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || s.poisoned {
		return Task{}, false, errors.New("Schedule service is not readable; reopen its storage")
	}
	task, found := s.tasks[id]
	if s.hostWide && !task.visibleTo(sessionID) {
		return Task{}, false, nil
	}
	return cloneTask(task), found, nil
}

// HistoryFor reads only the task row, never the original Session. The cursor
// is an exclusive message ID and pages follow append order, not wall time.
// A nil cursor selects the first page; a present empty cursor is not a match.
func (s *Service) HistoryFor(sessionID, id string, limit int, before *string) (HistoryPage, error) {
	return s.historyFor(sessionID, id, limit, before, false)
}

func (s *Service) historyFor(sessionID, id string, limit int, before *string, hostManagement bool) (HistoryPage, error) {
	if s.hostWide && sessionID == "" && !hostManagement {
		return HistoryPage{}, errors.New("Host Schedule requires a Session ID")
	}
	if limit < 1 || limit > 100 {
		return HistoryPage{}, errors.New("Schedule history limit must be from 1 through 100")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || s.poisoned {
		return HistoryPage{}, errors.New("Schedule service is not readable; reopen its storage")
	}
	result := HistoryPage{ID: id}
	task, found := s.tasks[id]
	if !found || (s.hostWide && !hostManagement && !task.visibleTo(sessionID)) {
		result.Code = "schedule_not_found"
		return result, nil
	}
	history := task.DeliveryHistory
	if history.Records == nil {
		history.EarlierRecordsUnavailable = true
		if task.LastDelivery != nil {
			history.Records = []Receipt{*task.LastDelivery}
		}
	}
	end := len(history.Records)
	if before != nil {
		end = -1
		for index, receipt := range history.Records {
			if receipt.MessageID == *before {
				end = index
				break
			}
		}
		if end < 0 {
			result.Code = "delivery_cursor_not_found"
			return result, nil
		}
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	result.Records = make([]Receipt, 0, end-start)
	for index := end - 1; index >= start; index-- {
		result.Records = append(result.Records, history.Records[index])
	}
	result.EarlierRecordsUnavailable = history.EarlierRecordsUnavailable
	result.EarlierRecordsPruned = history.EarlierRecordsPruned
	result.Retention.Days = s.limits.HistoryDays
	result.Retention.Records = s.limits.HistoryRecords
	if start > 0 {
		result.NextBefore = history.Records[start].MessageID
	}
	return result, nil
}

// HasActive reports whether this Host still has a timer obligation. Without
// an external wake service such a Host must not be reclaimed for idle time.
func (s *Service) HasActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range s.tasks {
		if task.Status == "active" {
			return true
		}
	}
	return false
}

func (s *Service) Delete(ctx context.Context, id string) (bool, error) {
	return s.DeleteFor(ctx, "", id)
}

func (s *Service) DeleteFor(ctx context.Context, sessionID, id string) (bool, error) {
	if s.hostWide && sessionID == "" {
		return false, errors.New("Host Schedule requires a Session ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || s.poisoned {
		return false, errors.New("Schedule service is not writable")
	}
	if s.tasks[id].Record.ID == "" || (s.hostWide && !s.tasks[id].visibleTo(sessionID)) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := s.mutateLocked(ctx, id, nil, true); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) Update(ctx context.Context, id string, expected Record, change func(Record, time.Time) (Record, error)) (Task, error) {
	return s.UpdateFor(ctx, "", id, expected, change)
}

func (s *Service) UpdateFor(ctx context.Context, sessionID, id string, expected Record, change func(Record, time.Time) (Record, error)) (Task, error) {
	return s.EditFor(ctx, sessionID, id, expected, "", change)
}

// EditFor updates a task and its activity under the same persistence lock.
func (s *Service) EditFor(ctx context.Context, sessionID, id string, expected Record, action string, change func(Record, time.Time) (Record, error)) (Task, error) {
	return s.EditForTarget(ctx, sessionID, id, expected, action, nil, change)
}

func (s *Service) EditForTarget(ctx context.Context, sessionID, id string, expected Record, action string, target *Target, change func(Record, time.Time) (Record, error)) (Task, error) {
	if change == nil {
		return Task{}, errors.New("Schedule update requires a change")
	}
	if s.hostWide && sessionID == "" {
		return Task{}, errors.New("Host Schedule requires a Session ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || s.poisoned {
		return Task{}, errors.New("Schedule service is not writable")
	}
	task, ok := s.tasks[id]
	if !ok || (s.hostWide && !task.visibleTo(sessionID)) {
		return Task{}, errors.New("schedule_not_found")
	}
	if (task.Status == "inactive" || task.Status == "ended") && action != "resume" {
		return Task{}, errors.New("schedule_ended")
	}
	if action != "" && action != "pause" && action != "resume" {
		return Task{}, errors.New("invalid Schedule activity")
	}
	if !reflect.DeepEqual(task.Record, expected) {
		return Task{}, errors.New("schedule_conflict")
	}
	return s.editLocked(ctx, task, sessionID, action, target, change, false)
}

func (s *Service) editLocked(ctx context.Context, task Task, actor, action string, target *Target, change func(Record, time.Time) (Record, error), rearm bool) (Task, error) {
	id := task.Record.ID
	status := task.Status
	switch action {
	case "pause":
		status = "paused"
	case "resume":
		status = "active"
	}
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	record, err := change(cloneTask(task).Record, s.now().UTC())
	if err != nil {
		return Task{}, err
	}
	if record.ID != id {
		return Task{}, errors.New("Schedule update cannot replace task identity")
	}
	changedTiming := !reflect.DeepEqual(recordTiming(record), recordTiming(task.Record)) || record.ScheduledAt != task.Record.ScheduledAt
	if rearm && changedTiming {
		task.PendingDelivery = nil
		if isEnded(task.Status) && action != "pause" {
			status = "active"
		}
	}
	if action == "resume" && task.Status != "active" {
		now := s.now().UTC()
		if taskDueAt(Task{Record: record}, now) {
			if record.Kind == "after" || record.Kind == "at" {
				return Task{}, errors.New("Schedule requires a future one-shot target to resume")
			}
			advance, err := ResolveRecurringOccurrence(record, now)
			if err != nil {
				return Task{}, err
			}
			if advance.NextScheduledAt == "" {
				return Task{}, errors.New("Schedule recurrence has no future occurrence")
			}
			record.ScheduledAt = advance.NextScheduledAt
		}
	}
	targetChanged := false
	if target != nil {
		if !s.hostWide {
			return Task{}, errors.New("Schedule targets require a Host service")
		}
		prior := task.Target
		if rearm {
			prior = nil
		}
		configured, err := s.configureTarget(ctx, *target, prior)
		if err != nil {
			return Task{}, err
		}
		if !reflect.DeepEqual(task.Target, configured) {
			task = cloneTask(task)
			fillReceiptTarget(&task, task.deliverySession())
			if configured.Kind != "new" && !task.visibleTo(configured.SessionID) {
				task.InvolvedSessionIDs = append(task.InvolvedSessionIDs, configured.SessionID)
			}
			task.Target, task.PendingDelivery = configured, nil
			targetChanged = true
		}
	}
	if (action == "resume" || changedTiming) &&
		s.checkSessionTarget != nil && task.Target != nil && task.Target.Kind == "session" {
		if err := s.checkSessionTarget(ctx, task.deliverySession()); err != nil {
			return Task{}, err
		}
	}
	if actor != "" && !task.visibleTo(actor) {
		task = cloneTask(task)
		task.InvolvedSessionIDs = append(task.InvolvedSessionIDs, actor)
		targetChanged = true
	}
	if !rearm && !targetChanged && reflect.DeepEqual(record, task.Record) && status == task.Status {
		return cloneTask(task), nil
	}
	if s.hostWide {
		task.UpdatedAt, err = nextRevision(task.UpdatedAt, s.now())
		if err != nil {
			return Task{}, err
		}
	}
	task.Record, task.Status = record, status
	if err := s.mutateLocked(ctx, id, &task, true); err != nil {
		return Task{}, err
	}
	s.queueNoticeLocked(task, actor, "updated")
	return cloneTask(task), nil
}

func (s *Service) run(ctx context.Context) {
	defer close(s.done)
	failed := map[string]bool{}
	for ctx.Err() == nil {
		s.mu.Lock()
		if s.poisoned {
			s.mu.Unlock()
			<-ctx.Done()
			return
		}
		if s.refreshExecutions(ctx) {
			failed = map[string]bool{}
		}
		var earliest time.Time
		for _, task := range s.tasks {
			if _, held := s.executing[task.Record.ID]; held || task.Status != "active" || failed[task.Record.ID] {
				continue
			}
			target, err := time.Parse("2006-01-02T15:04:05.000Z", task.Record.ScheduledAt)
			if err != nil {
				continue
			}
			if floor := s.nextAdmissionAt(task); floor.After(target) {
				target = floor
			}
			if earliest.IsZero() || target.Before(earliest) {
				earliest = target
			}
		}
		now := s.now()
		polling := false
		if len(s.executing) != 0 {
			poll := now.Add(time.Second)
			if earliest.IsZero() || earliest.After(poll) {
				earliest, polling = poll, true
			}
		}
		s.mu.Unlock()
		if earliest.IsZero() {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				failed = map[string]bool{}
				continue
			}
		}
		delay := earliest.Sub(now)
		if delay <= 0 {
			for id := range s.dispatchDue(ctx, failed) {
				failed[id] = true
			}
			continue
		}
		const maxTimer = 2147483647 * time.Millisecond
		if delay > maxTimer {
			delay = maxTimer
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
			failed = map[string]bool{}
		case <-timer.C:
			// A later timer starts a new drive, just like upstream requestDrive.
			// Only suppress failed tasks within the scan that reported failure;
			// another due obligation may provide the next retry opportunity.
			if !polling {
				failed = map[string]bool{}
			}
		}
	}
}

// The dispatch code is separate from the timer so its persistence-order and
// failure behavior can be tested without waiting for wall time.
func (s *Service) dispatchDue(ctx context.Context, skip map[string]bool) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	failed := map[string]bool{}
	if s.poisoned || s.dispatchStopped(ctx) {
		return failed
	}
	now := s.now().UTC()
	s.refreshExecutions(ctx)
	var due []Task
	for _, id := range s.order {
		task := s.tasks[id]
		if task.Status != "active" || skip[task.Record.ID] {
			continue
		}
		target, err := time.Parse("2006-01-02T15:04:05.000Z", task.Record.ScheduledAt)
		if err == nil && !target.After(now) && !s.nextAdmissionAt(task).After(now) && !s.executionPending(ctx, task) {
			due = append(due, cloneTask(task))
		}
	}
	handled := make(map[string]bool, len(due))
	for _, task := range due {
		if s.dispatchStopped(ctx) {
			return failed
		}
		if handled[task.Record.ID] {
			continue
		}
		handled[task.Record.ID] = true
		if task.Record.Kind == "after" || task.Record.Kind == "at" {
			prepared, sessionID, err := s.prepareTask(ctx, task)
			if err != nil {
				if taskDueAt(task, s.now()) {
					failed[task.Record.ID] = true
				}
				log.Printf("serveragent Schedule Session %s could not be prepared: %v", task.SessionID, err)
				continue
			}
			if s.dispatchStopped(ctx) {
				return failed
			}
			// Cold restoration may cross a wall-clock rollback. A task that is
			// no longer due keeps its existing target and timer obligation.
			if !taskDueAt(task, s.now()) {
				continue
			}
			text := reminderText(task, task.Record.ScheduledAt)
			if err := s.deliverAndCommit(ctx, prepared, sessionID, text, task.Record.ScheduledAt, ""); err != nil {
				failed[task.Record.ID] = true
				log.Printf("serveragent Schedule delivery %s was not acknowledged: %v", task.Record.ID, err)
				if s.poisoned {
					return failed
				}
			}
			continue
		}
		// The first due recurring task owns the batch's position in insertion
		// order. Other due recurring tasks in that Session join the same inbox
		// message even when a one-shot task lies between them in the table.
		group := []Task{task}
		for _, candidate := range due {
			if (task.Target == nil || task.Target.Kind == "session") &&
				(candidate.Target == nil || candidate.Target.Kind == "session") && candidate.deliverySession() == task.deliverySession() && candidate.Record.Kind != "after" && candidate.Record.Kind != "at" && !handled[candidate.Record.ID] {
				group = append(group, candidate)
				handled[candidate.Record.ID] = true
			}
		}
		prepared, sessionID, err := s.prepareTask(ctx, task)
		if err != nil {
			for _, member := range group {
				if taskDueAt(member, s.now()) {
					failed[member.Record.ID] = true
				}
			}
			log.Printf("serveragent Schedule Session %s could not be prepared: %v", task.SessionID, err)
			continue
		}
		if s.dispatchStopped(ctx) {
			return failed
		}
		if task.Target != nil && task.Target.Kind != "session" {
			now := s.now().UTC()
			if !taskDueAt(task, now) {
				continue
			}
			occurrence, err := ResolveRecurringOccurrence(task.Record, now)
			if err == nil {
				if occurrence.NextScheduledAt == "" {
					prepared.Record.ScheduledAt = occurrence.OccurrenceAt
				}
				err = s.deliverAndCommit(ctx, prepared, sessionID, reminderText(task, occurrence.OccurrenceAt), occurrence.OccurrenceAt, occurrence.NextScheduledAt)
			}
			if err != nil {
				failed[task.Record.ID] = true
				log.Printf("Schedule delivery %s was not acknowledged: %v", task.Record.ID, err)
			}
			if s.poisoned {
				return failed
			}
			continue
		}
		for id := range s.deliverRecurringGroup(ctx, sessionID, group, s.now().UTC()) {
			failed[id] = true
		}
		if s.poisoned {
			return failed
		}
	}
	return failed
}

// dispatchStopped is called only while mu is held. Close sets closing before
// waiting for that lock, so an in-flight scan can observe teardown promptly.
func (s *Service) dispatchStopped(ctx context.Context) bool {
	return ctx.Err() != nil || s.closing.Load() || s.closed
}

func (s *Service) prepareSession(ctx context.Context, sessionID string) error {
	if s.prepare == nil {
		return nil
	}
	return s.prepare(ctx, sessionID)
}

func taskDueAt(task Task, now time.Time) bool {
	target, err := time.Parse("2006-01-02T15:04:05.000Z", task.Record.ScheduledAt)
	return err == nil && !target.After(now)
}

func (s *Service) deliverRecurringGroup(ctx context.Context, sessionID string, recurring []Task, now time.Time) map[string]bool {
	failed := map[string]bool{}
	type reminder struct {
		ScheduleID     string `json:"schedule_id"`
		OccurrenceAt   string `json:"occurrence_at"`
		ReminderPrompt string `json:"reminder_prompt"`
	}
	items := make([]reminder, 0, len(recurring))
	advances := make([]Occurrence, 0, len(recurring))
	for _, task := range recurring {
		if !taskDueAt(task, now) {
			continue
		}
		occurrence, err := ResolveRecurringOccurrence(task.Record, now)
		if err != nil {
			failed[task.Record.ID] = true
			log.Printf("serveragent Schedule rule %s could not resolve its due occurrence: %v", task.Record.ID, err)
			continue
		}
		items = append(items, reminder{task.Record.ID, occurrence.OccurrenceAt, task.Record.Prompt})
		advances = append(advances, occurrence)
	}
	if len(items) == 0 {
		return failed
	}
	text := "[SCHEDULE REMINDER BATCH]\nThis is a scheduled message from the user\nreminders_json: " + encodeJSON(items)
	messageID, err := s.deliver(ctx, sessionID, text)
	if err != nil || messageID == "" {
		log.Printf("serveragent Schedule recurring batch was not acknowledged: %v", err)
		for _, item := range items {
			failed[item.ScheduleID] = true
		}
		return failed
	}
	for index, item := range items {
		task := s.tasks[item.ScheduleID]
		advance := advances[index]
		if advance.NextScheduledAt == "" {
			task.Status = "ended"
			task.Record.ScheduledAt = advance.OccurrenceAt
		} else {
			task.Record.ScheduledAt = advance.NextScheduledAt
		}
		if err := s.commitReceipt(ctx, task, sessionID, item.OccurrenceAt, messageID); err != nil {
			failed[item.ScheduleID] = true
			log.Printf("serveragent Schedule receipt %s was not committed: %v", item.ScheduleID, err)
			if s.poisoned {
				return failed
			}
		}
	}
	return failed
}

func (s *Service) deliverAndCommit(ctx context.Context, task Task, sessionID, text, occurrenceAt, next string) error {
	messageID, err := s.deliver(ctx, sessionID, text)
	if err != nil {
		return err
	}
	if messageID == "" {
		return errors.New("Schedule delivery returned no message ID")
	}
	if next == "" {
		task.Status = "ended"
	} else {
		task.Record.ScheduledAt = next
	}
	return s.commitReceipt(ctx, task, sessionID, occurrenceAt, messageID)
}

func (s *Service) commitReceipt(ctx context.Context, task Task, sessionID, occurrenceAt, messageID string) error {
	if messageID == "" {
		return errors.New("Schedule delivery returned no message ID")
	}
	held := s.observeExecution != nil && (task.Target == nil || task.Target.Kind != "new")
	when, err := canonicalUTC(s.now())
	if err != nil {
		return err
	}
	deliveredAt, err := time.Parse("2006-01-02T15:04:05.000Z", when)
	if err != nil {
		return err
	}
	receipt := Receipt{ScheduledAt: occurrenceAt, DeliveredAt: when, MessageID: messageID, TargetSessionID: sessionID}
	if task.Target != nil && task.Target.Kind == "new_once" {
		if !task.visibleTo(sessionID) {
			task.InvolvedSessionIDs = append(task.InvolvedSessionIDs, sessionID)
		}
		task.Target = &Target{Kind: "session", SessionID: sessionID}
	}
	task.PendingDelivery = nil
	history := make([]Receipt, 0, len(task.DeliveryHistory.Records)+2)
	if task.DeliveryHistory.Records == nil {
		// A pre-history task exposes its one legacy receipt. Preserve that
		// receipt before replacing LastDelivery and mark older records unknown.
		task.DeliveryHistory.EarlierRecordsUnavailable = true
		if task.LastDelivery != nil {
			history = append(history, *task.LastDelivery)
		}
	} else {
		history = append(history, task.DeliveryHistory.Records...)
	}
	task.LastDelivery = &receipt
	task.UpdatedAt, err = nextRevision(task.UpdatedAt, deliveredAt)
	if err != nil {
		return err
	}
	history = append(history, Receipt{
		ScheduledAt: occurrenceAt, DeliveredAt: when, MessageID: messageID, TargetSessionID: sessionID, Prompt: task.Record.Prompt,
	})
	floor := deliveredAt.Add(-time.Duration(s.limits.HistoryDays) * 24 * time.Hour)
	retained := history[:0]
	for _, item := range history {
		at, parseErr := time.Parse("2006-01-02T15:04:05.000Z", item.DeliveredAt)
		if parseErr == nil && !at.Before(floor) {
			retained = append(retained, item)
		}
	}
	if len(retained) > s.limits.HistoryRecords {
		retained = retained[len(retained)-s.limits.HistoryRecords:]
	}
	if len(retained) != len(history) {
		task.DeliveryHistory.EarlierRecordsUnavailable = true
		task.DeliveryHistory.EarlierRecordsPruned = true
	}
	task.DeliveryHistory.Records = retained
	if err := s.mutateLocked(ctx, task.Record.ID, &task, false); err != nil {
		return err
	}
	if held {
		s.executing[task.Record.ID] = receipt
	}
	return nil
}

func encodeJSON(value any) string {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimSuffix(output.String(), "\n")
}
