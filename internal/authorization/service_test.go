package authorization

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"go-bot/internal/credentials"
)

func fixture(t *testing.T) (*Service, *credentials.Store) {
	t.Helper()
	store, err := credentials.Open(filepath.Join(t.TempDir(), credentials.Filename), credentials.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(store), store
}
func register(t *testing.T, s *Service, key string, run func(*Session) error) func() {
	t.Helper()
	off, err := s.Register(Flow{Key: key, Label: "Account", Methods: []Method{{ID: "oauth", Label: "Sign in"}}, Run: run})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(off)
	return off
}
func TestAuthorizationRequiresObservedCommittedCredential(t *testing.T) {
	for _, kind := range []string{"stale", "commit", "direct", "same", "deleted"} {
		t.Run(kind, func(t *testing.T) {
			s, store := fixture(t)
			key := "mail/account"
			_, err := store.ModifyRecord(t.Context(), key, func(*credentials.Record) (*credentials.Record, error) {
				return &credentials.Record{Kind: "api-key"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			register(t, s, key, func(a *Session) error {
				if kind == "stale" {
					return nil
				}
				value := "new-key"
				record := &credentials.Record{Kind: "api-key", Key: &value}
				if kind == "same" {
					record.Key = nil
				}
				if kind == "direct" {
					_, err := store.ModifyRecord(t.Context(), key, func(*credentials.Record) (*credentials.Record, error) { return record, nil })
					return err
				}
				if err := a.Commit(record); err != nil {
					return err
				}
				if kind == "deleted" {
					return store.DeleteRecord(t.Context(), key)
				}
				return nil
			})
			out, err := s.Begin(t.Context(), key, "", Interaction{})
			if kind == "stale" || kind == "deleted" {
				var detail *Error
				if !errors.As(err, &detail) || detail.Code != "NOT_COMMITTED" {
					t.Fatal(out, err)
				}
			} else if err != nil || out.Status != "authorized" {
				t.Fatal(out, err)
			}
		})
	}
}
func TestAuthorizationDeclinePromptWithdrawalAndObserverIsolation(t *testing.T) {
	s, _ := fixture(t)
	register(t, s, "mail/decline", func(a *Session) error {
		_, err := a.Prompt(t.Context(), Prompt{Kind: "secret", Message: "Key"})
		if err != nil {
			return errors.New("provider wrapped decline")
		}
		return nil
	})
	off := s.Subscribe(func(key, status string) {
		if s.Describe(key).InFlight {
			t.Error("settlement fired before release")
		}
		panic("observer failure")
	})
	defer off()
	out, err := s.Begin(t.Context(), "mail/decline", "", Interaction{Prompt: func(context.Context, Prompt) (string, error) { return "", &Error{Code: "DECLINED", Message: "No"} }})
	if err != nil || out.Status != "cancelled" {
		t.Fatal(out, err)
	}
	register(t, s, "mail/withdrawn", func(a *Session) error {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := a.Prompt(ctx, Prompt{Kind: "text"}); err == nil {
			t.Error("withdrawn prompt ran")
		}
		return a.Commit(&credentials.Record{Kind: "api-key"})
	})
	out, err = s.Begin(t.Context(), "mail/withdrawn", "", Interaction{})
	if err != nil || out.Status != "authorized" {
		t.Fatal(out, err)
	}
}
func TestAuthorizationCancellationAndCommitAdmission(t *testing.T) {
	s, store := fixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	register(t, s, "mail/waiting", func(*Session) error { close(entered); <-release; return nil })
	result := make(chan Outcome, 1)
	go func() { out, _ := s.Begin(t.Context(), "mail/waiting", "", Interaction{}); result <- out }()
	<-entered
	_, err := s.Begin(t.Context(), "mail/waiting", "", Interaction{})
	var detail *Error
	if !errors.As(err, &detail) || detail.Code != "ALREADY_IN_FLIGHT" {
		t.Fatal(err)
	}
	s.Cancel("mail/waiting")
	select {
	case out := <-result:
		if out.Status != "cancelled" {
			t.Fatal(out)
		}
	case <-time.After(time.Second):
		t.Fatal("ignored cancellation kept credential busy")
	}
	close(release)
	lock, unlock := make(chan struct{}), make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		_, err := store.ModifyRecord(t.Context(), "mail/lock", func(*credentials.Record) (*credentials.Record, error) {
			close(lock)
			<-unlock
			return &credentials.Record{Kind: "api-key"}, nil
		})
		writeDone <- err
	}()
	<-lock
	register(t, s, "mail/commit", func(a *Session) error { return a.Commit(&credentials.Record{Kind: "api-key"}) })
	go func() { out, _ := s.Begin(t.Context(), "mail/commit", "", Interaction{}); result <- out }()
	deadline := time.Now().Add(time.Second)
	for {
		a := s.SessionByKey("mail/commit")
		if a != nil {
			a.mu.Lock()
			committing := a.committing
			a.mu.Unlock()
			if committing {
				break
			}
		}
		if time.Now().After(deadline) {
			close(unlock)
			t.Fatal("commit not admitted")
		}
		time.Sleep(time.Millisecond)
	}
	s.Cancel("mail/commit")
	if !s.Describe("mail/commit").InFlight {
		t.Fatal("admitted write was abandoned")
	}
	close(unlock)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if out := <-result; out.Status != "authorized" {
		t.Fatal(out)
	}
}
func (s *Service) SessionByKey(key string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[key]
}

func TestCallerCancellationDoesNotWaitForPropagation(t *testing.T) {
	caller, cancel := context.WithCancel(t.Context())
	// Model the interval before AfterFunc has propagated cancellation to the
	// session context. The caller's cancellation is already authoritative.
	a := &Session{Context: context.WithoutCancel(caller), caller: caller}
	cancel()
	if !a.withdrawn() {
		t.Fatal("withdrawal depends on asynchronous cancellation propagation")
	}
	if err := a.AdmitCommit(); !errors.Is(err, context.Canceled) {
		t.Fatalf("commit admitted after caller cancellation: %v", err)
	}
	if a.committing {
		t.Fatal("rejected commit crossed the write boundary")
	}
	// A write admitted before cancellation must still be allowed to settle.
	a.committing = true
	if a.withdrawn() {
		t.Fatal("caller cancellation abandoned an admitted write")
	}
	if err := a.AdmitCommit(); err != nil {
		t.Fatalf("admitted commit was withdrawn: %v", err)
	}
}
