package authorization

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-bot/internal/credentials"
)

func TestSurfaceResumesMultiplePromptsWithoutOwningClientConnection(t *testing.T) {
	service, _ := fixture(t)
	surface := NewSurface(t.Context(), service, nil)
	t.Cleanup(surface.Close)
	register(t, service, "mail/account", func(session *Session) error {
		answers := make(chan error, 2)
		for _, kind := range []string{"text", "secret"} {
			go func(kind string) {
				hint := ""
				if kind == "secret" {
					hint = "json-file"
				}
				_, err := session.Prompt(session.Context, Prompt{Kind: kind, Message: kind, WebInput: hint})
				answers <- err
			}(kind)
		}
		for range 2 {
			if err := <-answers; err != nil {
				return err
			}
		}
		key := "test-only"
		return session.Commit(&credentials.Record{Kind: "api-key", Key: &key})
	})
	initial, err := surface.start("mail/account", "oauth")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var view View
	for time.Now().Before(deadline) {
		value, err := surface.Handle(t.Context(), "get", json.RawMessage(`{"id":"`+initial.ID+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		view = value.(View)
		if len(view.Prompts) == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(view.Prompts) != 2 {
		t.Fatalf("pending prompts: %+v", view)
	}
	// The creating HTTP request can disappear. Another client discovers the
	// same attempt and pending questions using the normal list/get contract.
	value, err := surface.Handle(context.Background(), "list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	entries := value.([]EntryView)
	if len(entries) != 1 || entries[0].AttemptID != initial.ID || entries[0].AttemptStatus != "running" {
		t.Fatalf("cannot resume: %+v", entries)
	}
	for _, prompt := range view.Prompts {
		if prompt.Kind == "secret" && prompt.WebInput != "json-file" {
			t.Fatal("web presentation hint was lost")
		}
		input, _ := json.Marshal(map[string]string{"id": initial.ID, "promptId": prompt.ID, "answer": "local-test"})
		if _, err := surface.Handle(t.Context(), "answer", input); err != nil {
			t.Fatal(err)
		}
	}
	for time.Now().Before(deadline) {
		value, _ := surface.Handle(t.Context(), "list", json.RawMessage(`{}`))
		entries = value.([]EntryView)
		if entries[0].AttemptStatus == "authorized" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if entries[0].AttemptStatus != "authorized" || entries[0].InFlight || !entries[0].Configured || entries[0].AttemptID != initial.ID {
		t.Fatalf("completion not discoverable: %+v", entries)
	}
}

func TestSurfaceKeepsVisibleCallbackAnswersOutOfErrors(t *testing.T) {
	for _, kind := range []string{"text", "secret"} {
		t.Run(kind, func(t *testing.T) {
			service, _ := fixture(t)
			surface := NewSurface(t.Context(), service, nil)
			t.Cleanup(surface.Close)
			register(t, service, "native/account", func(session *Session) error {
				value, err := session.Prompt(session.Context, Prompt{Kind: kind, Message: "Callback URL"})
				if err != nil {
					return err
				}
				return fmt.Errorf("native error: %s", value)
			})
			initial, err := surface.start("native/account", "oauth")
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.After(3 * time.Second)
			var prompt PendingPrompt
			for prompt.ID == "" {
				value, err := surface.Handle(t.Context(), "get", json.RawMessage(`{"id":"`+initial.ID+`"}`))
				if err != nil {
					t.Fatal(err)
				}
				if prompts := value.(View).Prompts; len(prompts) > 0 {
					prompt = prompts[0]
					break
				}
				select {
				case <-deadline:
					t.Fatal("prompt did not appear")
				case <-time.After(time.Millisecond):
				}
			}
			answer := "http://localhost:8080/callback?code=private-one-time-code&state=attempt"
			input, _ := json.Marshal(map[string]string{"id": initial.ID, "promptId": prompt.ID, "answer": answer})
			if _, err := surface.Handle(t.Context(), "answer", input); err != nil {
				t.Fatal(err)
			}
			select {
			case <-surface.attempts[initial.ID].done:
			case <-deadline:
				t.Fatal("flow did not finish")
			}
			value, err := surface.Handle(t.Context(), "get", json.RawMessage(`{"id":"`+initial.ID+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(value)
			if strings.Contains(string(data), "private-one-time-code") || !strings.Contains(value.(View).Error, "[redacted]") {
				t.Fatal("authorization answer leaked through an error")
			}
		})
	}
}
