package sessionformat

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestNativeDurableActivityTracksInboxAndOpenTurn(t *testing.T) {
	ctx := context.Background()
	header := v4TestHeader("activity")
	s, err := NewLiveSession(header, &memoryV4Journal{})
	if err != nil {
		t.Fatal(err)
	}
	initial := s.DurableActivity()
	if !initial.Idle || !initial.Since.Equal(time.UnixMilli(header.CreatedAt).UTC()) {
		t.Fatalf("new activity = %+v", initial)
	}
	message := `{"id":"one","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"hello"}]}`
	appendEvent := func(kind, payload string) {
		t.Helper()
		if _, err := s.Append(EventInput{Type: kind, Data: json.RawMessage(payload)}); err != nil {
			t.Fatal(err)
		}
		if err := s.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent("agent/inbox/spliced", `{"target":"next-turn","start":0,"inserted":[`+message+`]}`)
	if snapshot := s.DurableActivity(); snapshot.Idle {
		t.Fatalf("queued input appeared idle: %+v", snapshot)
	}
	appendEvent("agent/inbox/spliced", `{"target":"next-turn","start":0,"removedCount":1,"inserted":[]}`)
	appendEvent("turn/start", `{"turn":1}`)
	if snapshot := s.DurableActivity(); snapshot.Idle {
		t.Fatalf("open turn appeared idle: %+v", snapshot)
	}
	appendEvent("turn/end", `{"turn":1,"reason":{"kind":"completed"}}`)
	if snapshot := s.DurableActivity(); !snapshot.Idle || snapshot.Since.Before(initial.Since) {
		t.Fatalf("completed turn activity = %+v", snapshot)
	}
}
