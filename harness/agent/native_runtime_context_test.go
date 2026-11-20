package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	sessionformat "go-bot/harness/session"
)

func TestWithdrawnRuntimeContextPublishesAnEmptyReplacement(t *testing.T) {
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "context-source", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &NativeLoop{session: live}
	sections := []RuntimeContextSection{{Name: "plugin:calendar", Text: "Connected calendar: work"}}
	loop.SetRuntimeContextSource(func(_ context.Context, id string) ([]RuntimeContextSection, error) {
		if id != "context-source" {
			t.Fatalf("wrong scope: %s", id)
		}
		return sections, nil
	})
	if err := loop.ensureRuntimeContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := loop.ensureRuntimeContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(live.Events()) != 1 {
		t.Fatal("Unchanged context should not append a duplicate")
	}
	sections = nil
	if err := loop.ensureRuntimeContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(live.Events()) != 2 {
		t.Fatal("Withdrawal did not supersede the previous snapshot")
	}
	var message struct {
		Source struct{ Sections []RuntimeContextSection }
	}
	if err := json.Unmarshal(live.Events()[1].Data, &message); err != nil {
		t.Fatal(err)
	}
	if len(message.Source.Sections) != 0 {
		t.Fatal("Withdrawn section survived")
	}
	loop.SetRuntimeContextSource(func(context.Context, string) ([]RuntimeContextSection, error) {
		return nil, errors.New("provider unavailable")
	})
	if err := loop.ensureRuntimeContext(t.Context()); err == nil {
		t.Fatal("Provider failure was hidden")
	}
}
