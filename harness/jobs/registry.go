package jobs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	DefaultMaxConcurrent = 10
	DefaultRetainBytes   = 256 << 10
)

var ErrCapacity = errors.New("background job capacity reached")

type Status string

const (
	Running   Status = "running"
	Stopping  Status = "stopping"
	Completed Status = "completed"
	Failed    Status = "failed"
	Killed    Status = "killed"
)

type Snapshot struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Label        string    `json:"label"`
	Status       Status    `json:"status"`
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt,omitempty"`
	ExitCode     int       `json:"exitCode,omitempty"`
	Error        string    `json:"error,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	RetainedFrom int64     `json:"retainedFrom"`
	OutputEnd    int64     `json:"outputEnd"`
}

type Output struct {
	Job    Snapshot `json:"job"`
	Text   string   `json:"text"`
	Cursor int64    `json:"cursor"`
	Lossy  bool     `json:"lossy"`
}

type Runner func(context.Context, func([]byte)) (int, error)

type record struct {
	snapshot Snapshot
	data     []byte
	cancel   context.CancelCauseFunc
	done     chan struct{}
	readFrom int64
	waiters  int
	visible  bool
	notified bool
}

// Registry is owned by one Agent worker. Jobs are process-local and never
// replayed from Session history after that worker disappears.
type Registry struct {
	mu       sync.Mutex
	noticeMu sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	jobs     map[string]*record
	max      int
	retain   int
	closed   bool
	onSettle func(Snapshot)
	settled  []Snapshot
	wake     chan struct{}
}

func New(parent context.Context, maxConcurrent, retainBytes int, onSettle func(Snapshot)) (*Registry, error) {
	if parent == nil || maxConcurrent <= 0 || retainBytes <= 0 {
		return nil, errors.New("invalid Job Registry configuration")
	}
	ctx, cancel := context.WithCancel(parent)
	return &Registry{ctx: ctx, cancel: cancel, jobs: make(map[string]*record), max: maxConcurrent, retain: retainBytes, onSettle: onSettle, wake: make(chan struct{}, 1)}, nil
}

func terminal(status Status) bool { return status == Completed || status == Failed || status == Killed }

// OutputLimit captures the visible producer buffer budget before a Job tool
// starts waiting or consumes output. It remains usable if the Job disappears.
func (r *Registry) OutputLimit(id string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, exists := r.jobs[id]
	return r.retain, exists
}

// Start reserves capacity before running producer code. A failed admission
// starts no goroutine or external effect.
func (r *Registry) Start(kind, label string, run Runner) (Snapshot, error) {
	if kind == "" || label == "" || run == nil {
		return Snapshot{}, errors.New("invalid background job")
	}
	r.mu.Lock()
	if r.closed || r.ctx.Err() != nil {
		r.mu.Unlock()
		return Snapshot{}, errors.New("Job Registry is closed")
	}
	active := 0
	for _, job := range r.jobs {
		if !terminal(job.snapshot.Status) {
			active++
		}
	}
	if active >= r.max {
		r.mu.Unlock()
		return Snapshot{}, fmt.Errorf("%w: background job limit %d reached; wait for or kill an unneeded job", ErrCapacity, r.max)
	}
	ctx, cancel := context.WithCancelCause(r.ctx)
	rec := &record{snapshot: Snapshot{ID: uuid.NewString(), Kind: kind, Label: label, Status: Running, StartedAt: time.Now().UTC()}, cancel: cancel, done: make(chan struct{})}
	r.jobs[rec.snapshot.ID] = rec
	snapshot := rec.snapshot
	r.mu.Unlock()
	go r.run(ctx, rec, run)
	return snapshot, nil
}

func (r *Registry) run(ctx context.Context, rec *record, runner Runner) {
	var exitCode int
	var err error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("background job producer panicked: %v", recovered)
			}
		}()
		exitCode, err = runner(ctx, func(chunk []byte) { r.append(rec, chunk) })
	}()
	r.mu.Lock()
	if rec.snapshot.Status == Stopping || ctx.Err() != nil {
		rec.snapshot.Status = Killed
	} else if err != nil {
		rec.snapshot.Status = Failed
	} else {
		rec.snapshot.Status = Completed
	}
	rec.snapshot.ExitCode = exitCode
	if err != nil {
		rec.snapshot.Error = err.Error()
	}
	rec.snapshot.FinishedAt = time.Now().UTC()
	snapshot := rec.snapshot
	if rec.visible && rec.waiters == 0 && !r.closed {
		r.enqueueSettled(rec)
	}
	close(rec.done)
	r.mu.Unlock()
	rec.cancel(nil)
	if r.onSettle != nil {
		r.onSettle(snapshot)
	}
}

func (r *Registry) enqueueSettled(rec *record) {
	if rec.notified {
		return
	}
	rec.notified = true
	r.settled = append(r.settled, rec.snapshot)
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// MarkVisible records that a Job ID is being returned by a tool. An already
// settled fast job is notified now; an invisible foreground completion is not.
func (r *Registry) MarkVisible(id string) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job := r.jobs[id]
	if job == nil {
		return Snapshot{}, errors.New("background job not found")
	}
	job.visible = true
	if terminal(job.snapshot.Status) && job.waiters == 0 && !r.closed {
		r.enqueueSettled(job)
	}
	return job.snapshot, nil
}

func (r *Registry) SettledWake() <-chan struct{} { return r.wake }

// PendingSettled does not consume notifications. The caller must first make
// each notification durable, then acknowledge it. A failed append cannot
// silently lose a completion wakeup.
func (r *Registry) PendingSettled() []Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Snapshot(nil), r.settled...)
}

// DeliverSettled serializes durable notice delivery with a model's terminal
// read or kill. A notice already being appended wins the ordering; otherwise
// SuppressNotice can remove it before delivery begins. Failed appends remain
// queued for the owner's failure handling rather than being acknowledged.
func (r *Registry) DeliverSettled(deliver func(Snapshot) error) error {
	r.noticeMu.Lock()
	defer r.noticeMu.Unlock()
	for {
		r.mu.Lock()
		if len(r.settled) == 0 {
			r.mu.Unlock()
			return nil
		}
		job := r.settled[0]
		r.mu.Unlock()
		if err := deliver(job); err != nil {
			return err
		}
		r.mu.Lock()
		r.settled = r.settled[1:]
		r.mu.Unlock()
	}
}

func (r *Registry) AckSettled(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index, snapshot := range r.settled {
		if snapshot.ID == id {
			r.settled = append(r.settled[:index], r.settled[index+1:]...)
			return
		}
	}
}

// SuppressNotice is used when a tool has already returned a terminal Job
// result or the Agent explicitly kills it. A queued but not yet delivered
// completion must not generate a redundant turn.
func (r *Registry) SuppressNotice(id string) {
	r.noticeMu.Lock()
	defer r.noticeMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if job := r.jobs[id]; job != nil {
		job.notified = true
	}
	for index := 0; index < len(r.settled); index++ {
		if r.settled[index].ID == id {
			r.settled = append(r.settled[:index], r.settled[index+1:]...)
			index--
		}
	}
}

func (r *Registry) append(rec *record, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if terminal(rec.snapshot.Status) {
		return
	}
	rec.snapshot.OutputEnd += int64(len(chunk))
	if len(chunk) >= r.retain {
		rec.data = append(rec.data[:0], chunk[len(chunk)-r.retain:]...)
	} else {
		rec.data = append(rec.data, chunk...)
		if len(rec.data) > r.retain {
			rec.data = append([]byte(nil), rec.data[len(rec.data)-r.retain:]...)
		}
	}
	rec.snapshot.RetainedFrom = rec.snapshot.OutputEnd - int64(len(rec.data))
}

func (r *Registry) List() []Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]Snapshot, 0, len(r.jobs))
	for _, job := range r.jobs {
		items = append(items, job.snapshot)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].StartedAt.Before(items[j].StartedAt) })
	return items
}

// Activity augments durable Session idleness while process-local jobs exist.
// It is not persisted: after worker death those jobs no longer exist.
func (r *Registry) Activity() (active bool, since time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A settled notification remains owner activity until its inbox event is
	// durably admitted. Otherwise idle reaping can win the completion race.
	active = len(r.settled) != 0
	for _, job := range r.jobs {
		if !terminal(job.snapshot.Status) {
			active = true
		}
		at := job.snapshot.FinishedAt
		if at.IsZero() {
			at = job.snapshot.StartedAt
		}
		if at.After(since) {
			since = at
		}
	}
	return active, since
}

func (r *Registry) Read(id string, cursor int64) (Output, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job := r.jobs[id]
	if job == nil || cursor < 0 || cursor > job.snapshot.OutputEnd {
		return Output{}, errors.New("background job or output cursor not found")
	}
	start := cursor
	lossy := start < job.snapshot.RetainedFrom
	if lossy {
		start = job.snapshot.RetainedFrom
	}
	return Output{Job: job.snapshot, Text: string(job.data[start-job.snapshot.RetainedFrom:]), Cursor: job.snapshot.OutputEnd, Lossy: lossy}, nil
}

// Consume advances only this Agent's model cursor. A prior foreground result
// can consume its partial output before the promoted job is exposed.
func (r *Registry) Consume(id string) (Output, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job := r.jobs[id]
	if job == nil {
		return Output{}, errors.New("background job not found")
	}
	start := job.readFrom
	lossy := start < job.snapshot.RetainedFrom
	if lossy {
		start = job.snapshot.RetainedFrom
	}
	output := Output{Job: job.snapshot, Text: string(job.data[start-job.snapshot.RetainedFrom:]), Cursor: job.snapshot.OutputEnd, Lossy: lossy}
	job.readFrom = output.Cursor
	return output, nil
}

// Remove is used only when a foreground call already returned the terminal
// result, so no model-visible Job ID needs to remain in the registry.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	job := r.jobs[id]
	if job == nil || !terminal(job.snapshot.Status) {
		return errors.New("only a settled background job can be removed")
	}
	delete(r.jobs, id)
	return nil
}

// Wait observes a Job for control-plane and foreground-shell purposes. It does
// not count as model delivery and cannot suppress a completion notice.
func (r *Registry) Wait(ctx context.Context, id string, timeout time.Duration) (Snapshot, error) {
	return r.wait(ctx, id, timeout, false)
}

// WaitForModel registers the model's live wait before settlement. If that wait
// receives the terminal status, the tool result owns delivery and no separate
// completion notice is queued, matching the upstream `awaited` settlement.
func (r *Registry) WaitForModel(ctx context.Context, id string, timeout time.Duration) (Snapshot, error) {
	return r.wait(ctx, id, timeout, true)
}

func (r *Registry) wait(ctx context.Context, id string, timeout time.Duration, model bool) (Snapshot, error) {
	if timeout < 0 {
		return Snapshot{}, errors.New("negative job wait timeout")
	}
	r.mu.Lock()
	job := r.jobs[id]
	if job == nil {
		r.mu.Unlock()
		return Snapshot{}, errors.New("background job not found")
	}
	if terminal(job.snapshot.Status) || timeout == 0 {
		snapshot := job.snapshot
		r.mu.Unlock()
		return snapshot, nil
	}
	if model {
		job.waiters++
	}
	done := job.done
	r.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var waitErr error
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
		waitErr = ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if model {
		job.waiters--
	}
	if model && terminal(job.snapshot.Status) {
		if waitErr == nil {
			// The successful wait's tool result carries the settlement.
			job.notified = true
		} else if job.waiters == 0 && job.visible && !job.notified && !r.closed {
			// Every waiter canceled before reporting a terminal result.
			r.enqueueSettled(job)
		}
	}
	if waitErr != nil {
		return Snapshot{}, waitErr
	}
	return job.snapshot, nil
}

func (r *Registry) Kill(id string) (Snapshot, error) {
	snapshot, _, err := r.kill(id, "", false)
	return snapshot, err
}

// RequestKill is the model-facing cancellation. It serializes the kill result
// and notice suppression with durable notice delivery; a racing settlement
// can therefore be observed either before this tool result or by this result,
// never as a stale notice appended afterward.
func (r *Registry) RequestKill(id, reason string) (Snapshot, string, error) {
	r.noticeMu.Lock()
	defer r.noticeMu.Unlock()
	return r.kill(id, reason, true)
}

func (r *Registry) kill(id, reason string, suppress bool) (Snapshot, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job := r.jobs[id]
	if job == nil {
		return Snapshot{}, "", errors.New("background job not found")
	}
	outcome := "already-finished"
	if !terminal(job.snapshot.Status) {
		outcome = "cancellation-requested"
		job.snapshot.Status = Stopping
		if reason != "" {
			job.snapshot.Detail = reason
		}
		cause := errors.New("Job cancellation requested")
		if reason != "" {
			cause = errors.New(reason)
		}
		job.cancel(cause)
	}
	if suppress {
		job.notified = true
		for index := 0; index < len(r.settled); index++ {
			if r.settled[index].ID == id {
				r.settled = append(r.settled[:index], r.settled[index+1:]...)
				index--
			}
		}
	}
	return job.snapshot, outcome, nil
}

// Close cancels all live jobs and waits for their producer cleanup. A caller
// deadline may stop waiting; the surrounding container must then be torn down.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	r.cancel()
	var pending []<-chan struct{}
	for _, job := range r.jobs {
		if !terminal(job.snapshot.Status) {
			job.snapshot.Status = Stopping
			pending = append(pending, job.done)
		}
	}
	r.mu.Unlock()
	for _, done := range pending {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.jobs = make(map[string]*record)
	r.mu.Unlock()
	return nil
}
