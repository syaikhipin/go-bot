package session

import (
	"context"
	"testing"
	"time"
)

type activityJournal struct{}

func (activityJournal) Append(context.Context, []Event) error { return nil }
func (activityJournal) Flush(context.Context) error           { return nil }
func (activityJournal) Close() error                          { return nil }

func TestDurableActivityIncrementallyProjectsOnlyFlushedEvents(t *testing.T) {
	header := appendStoreHeader()
	s, err := New(header, activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.DurableActivity()
	if err != nil || !initial.Idle || !initial.Since.Equal(header.CreatedAt) {
		t.Fatalf("initial activity = %#v, %v", initial, err)
	}
	if _, err := s.Append("user/inbox", 0, 0, map[string]string{"messageId": "m1"}); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := s.DurableActivity(); err != nil || !snapshot.Idle {
		t.Fatalf("unflushed input became visible: %#v, %v", snapshot, err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := s.DurableActivity(); err != nil || snapshot.Idle || s.activity.processed != 1 {
		t.Fatalf("queued input projection = %#v, %v; processed=%d", snapshot, err, s.activity.processed)
	}
	if _, err := s.Append("turn/start", 1, 0, map[string]string{"messageId": "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := s.DurableActivity(); err != nil || snapshot.Idle || s.activity.processed != 2 {
		t.Fatalf("running turn projection = %#v, %v; processed=%d", snapshot, err, s.activity.processed)
	}
	if _, err := s.Append("turn/end", 1, 0, map[string]string{"reason": "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	ended, err := s.DurableActivity()
	if err != nil || !ended.Idle || !ended.Since.Equal(s.events[2].At) || s.activity.processed != 3 {
		t.Fatalf("finished turn projection = %#v, %v; processed=%d", ended, err, s.activity.processed)
	}
	if again, err := s.DurableActivity(); err != nil || again != ended || s.activity.processed != 3 {
		t.Fatalf("unchanged prefix was reprojected: %#v, %v", again, err)
	}
	restored, err := Resume(header, s.DurableEvents(), activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, err := restored.DurableActivity(); err != nil || snapshot != ended {
		t.Fatalf("resumed activity = %#v, %v; want %#v", snapshot, err, ended)
	}
}

func TestActivityProjectionRefusesMalformedOrMismatchedTurn(t *testing.T) {
	header := appendStoreHeader()
	now := time.Now().UTC()
	for _, events := range [][]Event{
		{{Seq: 1, Type: "user/inbox", At: now, Data: []byte(`{}`)}},
		{{Seq: 1, Type: "turn/start", Turn: 1, At: now, Data: []byte(`{}`)}, {Seq: 2, Type: "turn/end", Turn: 2, At: now}},
		{{Seq: 1, Type: "turn/start", Turn: 1, At: now, Data: []byte(`{"messageId":"missing"}`)}, {Seq: 2, Type: "turn/end", Turn: 1, At: now, Data: []byte(`{"reason":"interrupted_before_step"}`)}},
	} {
		if snapshot, err := ProjectActivity(header, events); err == nil {
			t.Fatalf("malformed lifecycle was considered idle: %#v", snapshot)
		}
	}
}
