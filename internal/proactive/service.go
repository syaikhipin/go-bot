package proactive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"go-bot/internal/durablefs"
	"go-bot/internal/localstate"
)

type Storage interface {
	Load() (State, error)
	Save(State) error
}

type FileStorage struct{ Path string }

func (f FileStorage) Load() (State, error) {
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	err = json.Unmarshal(data, &state)
	return state, err
}

func (f FileStorage) Save(state State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := durablefs.EnsureDirectory(filepath.Dir(f.Path)); err != nil {
		return err
	}
	return localstate.WritePrivate(f.Path, data)
}

// Completed is accepted only after the ordinary Agent RunNext returned and
// its structured tool result and turn/end have committed to the journal.
type Completed struct {
	Turn        uint64
	CompletedAt int64
	Result      Result
}

// Hooks bind Host assembly and durable message admission to the scheduler.
// There is deliberately no step executor, tool dispatcher or child manager.
type Hooks struct {
	Prepare    func(context.Context, State) error
	Collect    func(context.Context, State) error
	Execute    func(context.Context, State, WakeReason) (Completed, error)
	Recover    func(context.Context, State) (*Completed, error)
	ReadResult func(context.Context, State, uint64) (Completed, error)
	Deliver    func(context.Context, State, Completed, string) error
}

type Service struct {
	mu            sync.Mutex
	state         State
	storage       Storage
	hooks         Hooks
	now           func() time.Time
	wake          chan struct{}
	enabled       bool
	configured    bool
	enablePending bool // a user off→on transition, not initial Host synchronization
	running       bool
	driving       bool
	cancel        context.CancelFunc
	changeVersion uint64 // process-local collection boundary, not an event queue
}

func NewService(storage Storage, hooks Hooks) (*Service, error) {
	if storage == nil || hooks.Prepare == nil || hooks.Collect == nil || hooks.Execute == nil || hooks.Deliver == nil || hooks.ReadResult == nil {
		return nil, errors.New("incomplete proactive Host integration")
	}
	state, err := storage.Load()
	if err != nil {
		return nil, err
	}
	if state.SessionID == "" {
		state.SessionID = uuid.NewString()
		if err := storage.Save(state); err != nil {
			return nil, err
		}
	}
	if uuid.Validate(state.SessionID) != nil || (state.MainSessionID != "" && uuid.Validate(state.MainSessionID) != nil) {
		return nil, errors.New("invalid proactive Session identity")
	}
	return &Service{state: state, storage: storage, hooks: hooks, now: time.Now, wake: make(chan struct{}, 1)}, nil
}

func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Configure receives account settings from Controller; they are not inferred
// from whether a chat SSE client happens to be connected.
func (s *Service) Configure(enabled bool, main string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	configured := s.configured
	if s.enabled == enabled && (main == "" || main == s.state.MainSessionID) {
		s.configured = true
		return nil // Repeated Controller synchronization is not a new trigger.
	}
	if main != "" {
		if uuid.Validate(main) != nil || (s.state.MainSessionID != "" && main != s.state.MainSessionID) {
			return errors.New("invalid proactive main Session target")
		}
		s.state.MainSessionID = main
	}
	s.configured = true
	if enabled && !s.enabled {
		s.state.LastError = ""
		s.enablePending = configured
	}
	s.enabled = enabled
	if !enabled {
		s.enablePending = false
	}
	if !enabled && s.cancel != nil {
		s.cancel()
	}
	err := s.storage.Save(s.state)
	s.signal()
	return err
}

// Changed stores only a wake opportunity and its time. Actual contents are
// collected later from their authoritative sources.
func (s *Service) Changed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changeVersion++
	s.state.Changed(s.now())
	if s.state.DeliveryMessageID != "" {
		s.state.LastError = ""
	}
	err := s.storage.Save(s.state)
	s.signal()
	return err
}

// ModelChanged allows a previously unavailable or invalid model to resume.
// It is not a public 'run now' trigger and does not invent a business timer.
func (s *Service) ModelChanged() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.LastError = ""
	err := s.storage.Save(s.state)
	s.signal()
	return err
}

func (s *Service) snapshotLocked() State {
	state := s.state
	if state.NextCheck != nil {
		value := *state.NextCheck
		state.NextCheck = &value
	}
	return state
}

func (s *Service) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := Status{Enabled: s.enabled, Phase: "waiting", LastRunStartedAt: s.state.LastRunStartedAt, LastCompletedAt: s.state.LastCompletedAt, Error: s.state.LastError}
	if s.state.NextCheck != nil {
		status.NextCheckAt, status.NextCheckReason = s.state.NextCheck.UnixMilli(), s.state.NextCheck.Reason
	}
	switch {
	case !s.enabled:
		status.Phase = "disabled"
	case s.running:
		status.Phase = "running"
	case s.state.LastError == "model_not_configured" || s.state.LastError == "model_selection_unavailable":
		status.Phase = "waiting-for-model"
	case s.state.LastError != "":
		status.Phase = "error"
	case s.state.MainSessionID == "":
		status.Phase = "waiting-for-main"
	}
	return status
}

func (s *Service) fail(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Runner errors must already be safe for the user's settings page. Do not
	// pass an HTTP response, secret-bearing command line or prompt as an error.
	s.state.LastError = err.Error()
	return s.storage.Save(s.state)
}

func (s *Service) apply(completed Completed) error {
	raw, err := json.Marshal(completed.Result)
	if err != nil {
		return err
	}
	if _, err := DecodeResult(raw, time.Time{}); err != nil || completed.Turn == 0 || completed.CompletedAt == 0 {
		return errors.New("background turn has no committed structured decision")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if completed.Turn <= s.state.ResultTurn {
		return nil
	}
	s.state.NextCheck = &completed.Result.NextCheck
	s.state.ResultTurn, s.state.LastCompletedAt, s.state.LastError = completed.Turn, completed.CompletedAt, ""
	if completed.Result.Notify != nil {
		s.state.DeliveryTurn = completed.Turn
		s.state.DeliveryMessageID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("go-bot/proactive/%s/%d", s.state.SessionID, completed.Turn))).String()
	}
	return s.storage.Save(s.state)
}

func (s *Service) deliver(ctx context.Context) error {
	s.mu.Lock()
	if !s.enabled {
		s.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	state := s.snapshotLocked()
	s.mu.Unlock()
	defer func() { cancel(); s.mu.Lock(); s.cancel = nil; s.mu.Unlock() }()
	if state.DeliveryMessageID == "" {
		return nil
	}
	completed, err := s.hooks.ReadResult(ctx, state, state.DeliveryTurn)
	if err != nil {
		return err
	}
	if completed.Result.Notify == nil {
		return errors.New("pending proactive delivery has no notification")
	}
	if err := s.hooks.Deliver(ctx, state, completed, state.DeliveryMessageID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.DeliveryMessageID, s.state.DeliveryTurn = "", 0
	return s.storage.Save(s.state)
}

func (s *Service) runOnce(ctx context.Context, reason WakeReason) error {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if !s.enabled || s.running || s.state.MainSessionID == "" {
		s.mu.Unlock()
		cancel()
		return nil
	}
	s.running, s.cancel = true, cancel
	s.enablePending = false
	s.state.LastRunStartedAt, s.state.LastError = s.now().UnixMilli(), ""
	state := s.snapshotLocked()
	err := s.storage.Save(s.state)
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.running, s.cancel = false, nil
		s.mu.Unlock()
	}()
	if err != nil {
		return err
	}
	if err := s.hooks.Prepare(ctx, state); err != nil {
		return err
	}
	s.mu.Lock()
	version := s.changeVersion
	s.mu.Unlock()
	if err := s.hooks.Collect(ctx, state); err != nil {
		return err
	}
	s.mu.Lock()
	if version == s.changeVersion {
		s.state.PendingChange = false
	}
	err = s.storage.Save(s.state)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	completed, err := s.hooks.Execute(ctx, state, reason)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.apply(completed)
}

// Run is the single external owner of this background Agent. Multiple trigger
// paths converge here; none can start concurrent turns or steer a live turn.
func (s *Service) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.driving {
		s.mu.Unlock()
		return errors.New("proactive service already has a driver")
	}
	s.driving = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.driving = false
		s.mu.Unlock()
	}()
	if s.hooks.Recover != nil {
		completed, err := s.hooks.Recover(ctx, s.Snapshot())
		if err != nil {
			if err := s.fail(err); err != nil {
				return err
			}
		} else if completed != nil {
			if err := s.apply(*completed); err != nil {
				return err
			}
		}
	}
	for ctx.Err() == nil {
		s.mu.Lock()
		state, enabled, enablePending := s.snapshotLocked(), s.enabled, s.enablePending
		s.mu.Unlock()
		if !enabled || state.MainSessionID == "" {
			select {
			case <-ctx.Done():
			case <-s.wake:
			}
			continue
		}
		if state.DeliveryMessageID != "" {
			if state.LastError != "" {
				select {
				case <-ctx.Done():
				case <-s.wake:
				}
				continue
			}
			if err := s.deliver(ctx); err != nil {
				s.mu.Lock()
				enabled := s.enabled
				s.mu.Unlock()
				if ctx.Err() != nil {
					return nil
				}
				if enabled {
					if err := s.fail(err); err != nil {
						return err
					}
				}
			}
			continue
		}
		at, reason := state.NextWake()
		if enablePending {
			at, reason = s.now(), EnabledWake
		} else if state.LastError == "" {
			switch {
			case state.LastRunStartedAt > state.LastCompletedAt:
				at, reason = s.now(), RecoverWake
			case state.NextCheck == nil:
				at, reason = s.now(), InitialWake
			}
		}
		if at.IsZero() {
			select {
			case <-ctx.Done():
			case <-s.wake:
			}
			continue
		}
		timer := time.NewTimer(max(0, at.Sub(s.now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			continue
		case <-s.wake:
			timer.Stop()
			continue
		case <-timer.C:
		}
		if err := s.runOnce(ctx, reason); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.mu.Lock()
			enabled := s.enabled
			s.mu.Unlock()
			if enabled {
				if err := s.fail(err); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
