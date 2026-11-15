package session

import (
	"context"
	"testing"
)

func TestTodoProjectionUsesOnlyDurableEventsAndRejectsBadReplay(t *testing.T) {
	s, err := New(appendStoreHeader(), activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		kind string
		step uint64
		data any
	}{
		{"turn/start", 0, map[string]string{}},
		{"step/start", 1, map[string]string{}},
		{"todo/write", 1, TodoSnapshot{Todos: []TodoItem{{Content: "draft", Status: "in_progress"}}}},
	} {
		if _, err := s.Append(entry.kind, 1, entry.step, entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if got, visible, err := s.Todos(); err != nil || visible || got != nil {
		t.Fatalf("unflushed todo leaked: %#v visible=%t err=%v", got, visible, err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, visible, err := s.Todos(); err != nil || !visible || len(got) != 1 || got[0].Content != "draft" {
		t.Fatalf("durable todo missing: %#v visible=%t err=%v", got, visible, err)
	}
	bad := append([]Event(nil), s.DurableEvents()...)
	bad[len(bad)-1].Step = 2
	resumed, err := Resume(s.Header(), bad, activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.ValidateTodos(); err == nil {
		t.Fatal("accepted todo/write outside its active step")
	}
}

func TestTodoReplayRejectsNonCanonicalSnapshot(t *testing.T) {
	s, err := New(appendStoreHeader(), activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		kind string
		step uint64
		data any
	}{
		{"turn/start", 0, map[string]string{}},
		{"step/start", 1, map[string]string{}},
		{"todo/write", 1, TodoSnapshot{Todos: []TodoItem{{Content: " draft ", Status: "pending"}}}},
	} {
		if _, err := s.Append(entry.kind, 1, entry.step, entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateTodos(); err == nil {
		t.Fatal("accepted a persisted todo snapshot that would silently change on replay")
	}
}
