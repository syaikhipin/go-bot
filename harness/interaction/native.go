package interaction

import (
	"context"
	"errors"
	"sync"
	"time"
)

// NativeQuestions is the process-local foreground answer channel. The AgentLoop
// persists tool/call before Ask; the eventual tool/result carries either an
// answer batch or the timed pending value. Continued questions are recovered
// from v4 events, without custom question/pending or question/answer events.
type NativeQuestions struct {
	mu               sync.Mutex
	enabled          bool
	approvalClients  int
	approvalAudience func() bool
	closed           bool
	waits            map[string]*nativeQuestionWait
}

type nativeQuestionWait struct {
	pending   Pending
	result    chan settled
	done      bool
	continued bool
}

func NewNativeQuestions() *NativeQuestions {
	return &NativeQuestions{waits: make(map[string]*nativeQuestionWait)}
}

func (s *NativeQuestions) Enable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.enabled = true
	}
}

func (s *NativeQuestions) Disable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = false
	for _, pending := range s.waits {
		if !pending.done {
			return true
		}
	}
	return false
}

func (s *NativeQuestions) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.enabled = false
	for id, pending := range s.waits {
		if !pending.done {
			pending.done = true
			pending.result <- settled{err: ErrUnavailable}
		}
		delete(s.waits, id)
	}
}

func (s *NativeQuestions) Pending() []Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Pending, 0, len(s.waits))
	for _, pending := range s.waits {
		if !pending.done || pending.continued {
			copy := pending.pending
			copy.Questions = append([]Question(nil), copy.Questions...)
			result = append(result, copy)
		}
	}
	return result
}

func (s *NativeQuestions) Ask(ctx context.Context, turn, step uint64, callID string, questions []Question) (Answer, error) {
	answer, _, err := s.ask(ctx, turn, step, callID, questions, 0, nil)
	return answer, err
}

// AskTimed releases only the foreground tool call after timeout. The question
// remains answerable through a later user message, not through the old call.
func (s *NativeQuestions) AskTimed(ctx context.Context, turn, step uint64, callID string, questions []Question, timeout time.Duration) (Answer, bool, error) {
	if timeout <= 0 {
		return Answer{}, false, errors.New("timed question requires a positive timeout")
	}
	return s.ask(ctx, turn, step, callID, questions, timeout, nil)
}

func (s *NativeQuestions) SetApprovalAudience(audience func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approvalAudience = audience
}

// AttachApprovalClient marks a live interactive session stream.
func (s *NativeQuestions) AttachApprovalClient() func() {
	s.mu.Lock()
	s.approvalClients++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.approvalClients--
		s.mu.Unlock()
	}
}

func (s *NativeQuestions) AskApproval(ctx context.Context, turn, step uint64, id string, approval Approval, questions []Question) (Answer, error) {
	answer, _, err := s.ask(ctx, turn, step, id, questions, 0, &approval)
	return answer, err
}

func (s *NativeQuestions) ask(ctx context.Context, turn, step uint64, callID string, questions []Question, timeout time.Duration, approval *Approval) (Answer, bool, error) {
	if turn == 0 || step == 0 || callID == "" {
		return Answer{}, false, errors.New("question requires a running tool call")
	}
	if err := ValidateQuestions(questions); err != nil {
		return Answer{}, false, err
	}
	s.mu.Lock()
	if !s.enabled || s.closed || (approval != nil && s.approvalClients == 0 && (s.approvalAudience == nil || !s.approvalAudience())) {
		s.mu.Unlock()
		return Answer{}, false, ErrUnavailable
	}
	if s.waits[callID] != nil {
		s.mu.Unlock()
		return Answer{}, false, errors.New("question call is already pending")
	}
	w := &nativeQuestionWait{
		pending: Pending{Approval: approval, CallID: callID, Questions: append([]Question(nil), questions...), Turn: turn, Step: step, State: "open"},
		result:  make(chan settled, 1),
	}
	s.waits[callID] = w
	s.mu.Unlock()
	var timer *time.Timer
	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timeoutCh = timer.C
		defer timer.Stop()
	}
	select {
	case result := <-w.result:
		s.mu.Lock()
		delete(s.waits, callID)
		s.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return Answer{}, false, err
		}
		return result.answer, false, result.err
	case <-timeoutCh:
		s.mu.Lock()
		if w.done {
			delete(s.waits, callID)
			s.mu.Unlock()
			result := <-w.result
			if err := ctx.Err(); err != nil {
				return Answer{}, false, err
			}
			return result.answer, false, result.err
		}
		if err := ctx.Err(); err != nil {
			delete(s.waits, callID)
			s.mu.Unlock()
			return Answer{}, false, err
		}
		w.done = true
		w.continued = true
		w.pending.State = "continued"
		s.mu.Unlock()
		return Answer{}, true, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.waits, callID)
		s.mu.Unlock()
		return Answer{}, false, ctx.Err()
	}
}

func (s *NativeQuestions) Answer(_ context.Context, callID string, answer Answer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.waits[callID]
	if w == nil {
		return ErrNotPending
	}
	if w.done {
		if w.continued {
			return ErrContinued
		}
		return ErrAlreadyAnswered
	}
	if w.pending.Approval != nil &&
		(len(answer.Answers) != 1 || answer.Answers[0].Custom != nil || len(answer.Answers[0].Selected) != 1) {
		return ErrInvalidAnswer
	}
	if err := validateAnswer(w.pending.Questions, answer); err != nil {
		return err
	}
	w.done = true
	w.result <- settled{answer: normalizeAnswer(answer)}
	return nil
}

func (s *NativeQuestions) Cancel(_ context.Context, callID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.waits[callID]
	if w == nil {
		return ErrNotPending
	}
	if w.done {
		if w.continued {
			delete(s.waits, callID)
			return nil
		}
		return ErrAlreadyAnswered
	}
	w.done = true
	w.result <- settled{err: ErrCancelled}
	return nil
}
