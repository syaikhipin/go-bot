package authorization

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestNativeAuthorizationDoesNotDuplicateCredentials(t *testing.T) {
	s, store := fixture(t)
	var configured atomic.Bool
	off, err := s.Register(Flow{Key: "native/account", Label: "Native CLI", Methods: []Method{{ID: "login", Label: "Sign in"}},
		Run: func(*Session) error { configured.Store(true); return nil },
		Inspect: func(context.Context) (AccountState, error) {
			return AccountState{Configured: configured.Load(), AccountLabel: "example"}, nil
		},
		Disconnect: func(context.Context) error { configured.Store(false); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer off()
	result, err := s.Begin(t.Context(), "native/account", "login", Interaction{})
	if err != nil || result.Status != "authorized" {
		t.Fatal(result, err)
	}
	info, err := store.DescribeRecord("native/account")
	if err != nil || info.Configured {
		t.Fatal("native grant was copied into Go storage", info, err)
	}
	state, err := s.Inspect(t.Context(), "native/account")
	if err != nil || !state.Configured || state.AccountLabel != "example" {
		t.Fatal(state, err)
	}
	state, err = s.Disconnect(t.Context(), "native/account")
	if err != nil || state.Configured {
		t.Fatal(state, err)
	}
}
func TestNativeAuthorizationRequiresInspectConfirmation(t *testing.T) {
	s, _ := fixture(t)
	off, err := s.Register(Flow{Key: "native/account", Label: "Native", Methods: []Method{{ID: "login", Label: "Sign in"}}, Run: func(*Session) error { return nil }, Inspect: func(context.Context) (AccountState, error) { return AccountState{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer off()
	_, err = s.Begin(t.Context(), "native/account", "login", Interaction{})
	var detail *Error
	if !errors.As(err, &detail) || detail.Code != "NOT_CONFIGURED" {
		t.Fatal(err)
	}
}
func TestNativeDisconnectExcludesConcurrentLogin(t *testing.T) {
	s, _ := fixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	off, err := s.Register(Flow{Key: "native/account", Label: "Native", Methods: []Method{{ID: "login", Label: "Sign in"}}, Run: func(*Session) error { return nil }, Inspect: func(context.Context) (AccountState, error) { return AccountState{}, nil }, Disconnect: func(context.Context) error { close(entered); <-release; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer off()
	done := make(chan error, 1)
	go func() { _, err := s.Disconnect(t.Context(), "native/account"); done <- err }()
	<-entered
	_, err = s.Begin(t.Context(), "native/account", "login", Interaction{})
	close(release)
	if final := <-done; final != nil {
		t.Fatal(final)
	}
	var detail *Error
	if !errors.As(err, &detail) || detail.Code != "ALREADY_IN_FLIGHT" {
		t.Fatal(err)
	}
}
