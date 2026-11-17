package hostruntime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sync"
	"time"

	"go-bot/harness/schedule"
	"go-bot/harness/session"
)

var (
	ErrHostClosed          = errors.New("native Host is closed")
	ErrHostSessionActive   = errors.New("native Session is already active")
	ErrHostSessionArchived = errors.New("native Session is archived")
	ErrHostSessionBusy     = errors.New("native Session is busy")
	ErrHostSessionAbsent   = errors.New("native Session is not active")
	hostSessionID          = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// NativeHost owns the top-level Session runtimes of one user. It does not own
// their journals or workspaces: Build binds each Session to its own durable
// storage and initial cwd. Child Agents remain inside their parent's runtime.
// Admission is serialized, while active Session turns run independently.
type NativeHost struct {
	UserID               string
	Mode                 string
	Build                func(sessionID string, resume bool) (*NativeRuntime, error)
	BuildInWorkspace     func(sessionID string, create schedule.TargetCreate) (*NativeRuntime, error)
	List                 func(context.Context, string, int) (sessionformat.SessionListPage, error)
	LatestSession        func(context.Context) (sessionformat.SessionListPage, error)
	ReadSession          func(context.Context, string) (sessionformat.PhysicalArtifact, error)
	ReadSessionHeader    func(context.Context, string) (sessionformat.Header, error)
	SessionLogInfo       func(string) (os.FileInfo, error)
	WorkspaceForSession  func(string) string
	Archived             func(string) (bool, error)
	SaveArchive          func(context.Context, string) error
	ReadTitle            func(string) string
	SaveTitle            func(string, string) error
	Schedule             *schedule.Service
	StartedAt            time.Time
	ReserveScheduleModel func(string) error

	approvalMu      sync.Mutex
	approvalStreams map[string]int
	operationMu     sync.Mutex
	historyMu       sync.RWMutex // Keeps cold history readers outside Session activation.
	scheduleClose   sync.Once
	mu              sync.Mutex
	sessions        map[string]*NativeRuntime
	activating      bool
	closed          bool
	lastActivity    time.Time
}

func (h *NativeHost) Activate(ctx context.Context, sessionID string, resume bool) (*NativeRuntime, error) {
	return h.activate(ctx, sessionID, resume, nil)
}

func (h *NativeHost) ActivateInWorkspace(ctx context.Context, sessionID string, create schedule.TargetCreate) (*NativeRuntime, error) {
	return h.activate(ctx, sessionID, false, &create)
}

func (h *NativeHost) activate(ctx context.Context, sessionID string, resume bool, create *schedule.TargetCreate) (*NativeRuntime, error) {
	if h.UserID == "" || h.Build == nil || !hostSessionID.MatchString(sessionID) {
		return nil, errors.New("invalid native Host Session activation")
	}
	h.operationMu.Lock()
	defer h.operationMu.Unlock()
	if err := h.CheckScheduleTarget(ctx, sessionID); err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrHostClosed
	}
	if h.sessions[sessionID] != nil {
		h.mu.Unlock()
		return nil, ErrHostSessionActive
	}
	h.activating = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.activating = false
		h.mu.Unlock()
	}()
	h.historyMu.Lock()
	defer h.historyMu.Unlock()
	var runtime *NativeRuntime
	var err error
	if create != nil && h.BuildInWorkspace != nil {
		runtime, err = h.BuildInWorkspace(sessionID, *create)
	} else {
		runtime, err = h.Build(sessionID, resume)
	}
	if err != nil {
		return nil, err
	}
	if runtime == nil || runtime.Identity.UserID != h.UserID || runtime.Identity.SessionID != sessionID {
		return nil, errors.New("native Host builder returned a foreign Session")
	}
	runtime.approvalAudience = func() bool { return h.hasChatStream(sessionID) }
	if err := runtime.Start(ctx, resume); err != nil {
		return nil, fmt.Errorf("activate native Session %s: %w", sessionID, err)
	}
	h.mu.Lock()
	if h.sessions == nil {
		h.sessions = make(map[string]*NativeRuntime)
	}
	h.sessions[sessionID] = runtime
	h.lastActivity = time.Now().UTC()
	h.mu.Unlock()
	return runtime, nil
}

// WakeSessions rechecks queued work after managed model settings change.
func (h *NativeHost) WakeSessions() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, runtime := range h.sessions {
		runtime.kick()
	}
}

// AttachChatStream tracks interactive delivery even while the Session is cold.
func (h *NativeHost) AttachChatStream(sessionID string) func() {
	h.approvalMu.Lock()
	if h.approvalStreams == nil {
		h.approvalStreams = make(map[string]int)
	}
	h.approvalStreams[sessionID]++
	h.approvalMu.Unlock()
	return func() {
		h.approvalMu.Lock()
		defer h.approvalMu.Unlock()
		if h.approvalStreams[sessionID] <= 1 {
			delete(h.approvalStreams, sessionID)
			return
		}
		h.approvalStreams[sessionID]--
	}
}

func (h *NativeHost) hasChatStream(sessionID string) bool {
	h.approvalMu.Lock()
	defer h.approvalMu.Unlock()
	return h.approvalStreams[sessionID] > 0
}

func (h *NativeHost) Session(sessionID string) (*NativeRuntime, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false
	}
	runtime, ok := h.sessions[sessionID]
	return runtime, ok
}

// WithSessionHistory keeps a cold read outside the interval where a runtime
// may be writing its journal but has not yet been published to Session().
func (h *NativeHost) WithSessionHistory(read func() error) error {
	h.historyMu.RLock()
	defer h.historyMu.RUnlock()
	return read()
}

// PrepareSchedule resolves the original Session before Schedule samples the
// final dispatch time. A cold Session is resumed from its own v4 journal;
// a missing or corrupt journal never creates a replacement Session.
func (h *NativeHost) PrepareSchedule(ctx context.Context, sessionID string) error {
	_, err := h.scheduleRuntime(ctx, sessionID)
	return err
}

func (h *NativeHost) scheduleRuntime(ctx context.Context, sessionID string) (*NativeRuntime, error) {
	runtime, found := h.Session(sessionID)
	if !found {
		var err error
		runtime, err = h.Activate(ctx, sessionID, true)
		if errors.Is(err, ErrHostSessionActive) {
			runtime, found = h.Session(sessionID)
			if found {
				err = nil
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return runtime, nil
}

// EnsureScheduleSession creates a reserved destination only when its journal
// is absent. Failed activation of an existing journal remains an error.
func (h *NativeHost) EnsureScheduleSession(ctx context.Context, sessionID string) error {
	return h.EnsureScheduleDestination(ctx, sessionID, schedule.TargetCreate{}, "")
}

func (h *NativeHost) EnsureScheduleDestination(ctx context.Context, sessionID string, create schedule.TargetCreate, title string) error {
	if err := h.PrepareSchedule(ctx, sessionID); errors.Is(err, os.ErrNotExist) {
		if h.ReserveScheduleModel != nil {
			if err := h.ReserveScheduleModel(sessionID); err != nil {
				return err
			}
		}
		if _, err := h.ActivateInWorkspace(ctx, sessionID, create); err != nil && !errors.Is(err, ErrHostSessionActive) {
			return err
		}
	} else if err != nil {
		return err
	}
	if h.SaveTitle != nil && title != "" {
		if err := h.SaveTitle(sessionID, title); err != nil {
			log.Printf("Schedule: name Session %s: %v", sessionID, err)
		}
	}
	return nil
}

// DeliverSchedule appends the prepared reminder to the original Session.
func (h *NativeHost) DeliverSchedule(ctx context.Context, sessionID, text string) (string, error) {
	runtime, err := h.scheduleRuntime(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if runtime.Factory.ResolveTurnModel != nil {
		if _, err := runtime.Factory.ResolveTurnModel(ctx); err != nil {
			return "", err
		}
	}
	return runtime.DeliverSchedule(ctx, text)
}

// StopSession drains only this top-level Session and its own children/jobs.
// Other Session runtimes remain live and continue accepting messages.
func (h *NativeHost) StopSession(ctx context.Context, sessionID string) error {
	h.operationMu.Lock()
	defer h.operationMu.Unlock()
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrHostClosed
	}
	runtime := h.sessions[sessionID]
	if runtime == nil {
		h.mu.Unlock()
		return ErrHostSessionAbsent
	}
	h.mu.Unlock()
	if err := runtime.Stop(ctx); err != nil {
		// A timed-out stop may still be draining its turn or child Jobs. Keep
		// the runtime visible so Host activity cannot report false idleness.
		return err
	}
	h.mu.Lock()
	delete(h.sessions, sessionID)
	h.lastActivity = time.Now().UTC()
	h.mu.Unlock()
	return nil
}

func (h *NativeHost) Activity() (sessionformat.ActivitySnapshot, error) {
	// A one-shot Schedule can cold-activate its Session and become inactive
	// while Activity is collecting runtimes. Check the timer obligation before
	// taking the Session snapshot as well as after it: otherwise the snapshot
	// can omit the newly activated runtime and report a false idle Host.
	scheduledBefore := h.Schedule != nil && h.Schedule.HasActive()
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return sessionformat.ActivitySnapshot{}, ErrHostClosed
	}
	runtimes := make([]*NativeRuntime, 0, len(h.sessions))
	activating := h.activating
	since := h.lastActivity
	if h.StartedAt.After(since) {
		since = h.StartedAt
	}
	for _, runtime := range h.sessions {
		runtimes = append(runtimes, runtime)
	}
	h.mu.Unlock()
	result := sessionformat.ActivitySnapshot{Idle: !activating, Since: since}
	for _, runtime := range runtimes {
		activity, err := runtime.Activity()
		if err != nil {
			return sessionformat.ActivitySnapshot{}, err
		}
		if !activity.Idle {
			result.Idle = false
		}
		if activity.Since.After(result.Since) {
			result.Since = activity.Since
		}
	}
	scheduledAfter := h.Schedule != nil && h.Schedule.HasActive()
	if scheduledBefore || scheduledAfter {
		result.Idle = false
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return sessionformat.ActivitySnapshot{}, ErrHostClosed
	}
	if h.lastActivity.After(result.Since) {
		result.Since = h.lastActivity
		result.Idle = false
	}
	if h.activating {
		result.Idle = false
	}
	h.mu.Unlock()
	return result, nil
}

// Close drains every Session. A caller deadline aborts unfinished runtimes,
// but keeps their handles so a later Close can finish their resource cleanup.
// An unfinished inbox is recovered from its own journal on the next resume.
func (h *NativeHost) Close(ctx context.Context) error {
	h.operationMu.Lock()
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.operationMu.Unlock()
	// A due Schedule delivery may be waiting to activate a Session. Release
	// operationMu before waiting for that dispatcher to stop; closed admission
	// makes any late activation fail without constructing another runtime.
	h.scheduleClose.Do(func() {
		if h.Schedule != nil {
			h.Schedule.Close()
		}
	})
	h.operationMu.Lock()
	defer h.operationMu.Unlock()
	h.mu.Lock()
	runtimes := make(map[string]*NativeRuntime, len(h.sessions))
	for id, runtime := range h.sessions {
		runtimes[id] = runtime
	}
	h.mu.Unlock()
	var failures []error
	for id, runtime := range runtimes {
		if err := runtime.Stop(ctx); err != nil {
			failures = append(failures, fmt.Errorf("stop Session %s: %w", id, err))
			continue
		}
		h.mu.Lock()
		delete(h.sessions, id)
		h.mu.Unlock()
	}
	return errors.Join(failures...)
}
