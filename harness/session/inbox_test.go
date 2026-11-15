package sessionformat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func v4InboxEvent(seq uint64, data string) Event {
	return Event{Type: "agent/inbox/spliced", Seq: seq, Time: int64(seq + 1), Data: json.RawMessage(data)}
}

func TestV4InboxProjectionReplaysAndRejectsDuplicatePendingIDs(t *testing.T) {
	first := `{"id":"one","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"first"}]}`
	second := `{"id":"two","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"second"}]}`
	state, err := FoldInbox([]Event{
		v4InboxEvent(0, `{"target":"next-turn","start":0,"inserted":[`+first+`]}`),
		v4InboxEvent(1, `{"target":"next-step","start":0,"inserted":[`+second+`]}`),
	})
	if err != nil || len(state.NextTurn) != 1 || len(state.NextStep) != 1 {
		t.Fatalf("inbox replay = %+v, %v", state, err)
	}
	duplicate := v4InboxEvent(2, `{"target":"next-turn","start":1,"inserted":[`+second+`]}`)
	if _, err := state.Next(duplicate); err == nil || !strings.Contains(err.Error(), "already pending") {
		t.Fatalf("duplicate pending ID accepted: %v", err)
	}
	if len(state.NextTurn) != 1 || len(state.NextStep) != 1 {
		t.Fatalf("rejected splice mutated source state: %+v", state)
	}
	claimed, err := state.Next(v4InboxEvent(2, `{"target":"next-turn","start":0,"removedCount":1,"inserted":[]}`))
	if err != nil || len(claimed.NextTurn) != 0 || len(claimed.NextStep) != 1 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	steered, err := state.Next(v4InboxEvent(2, `{"target":"next-turn","start":0,"removedCount":1,"inserted":[],"moveToNextStep":true}`))
	if err != nil || len(steered.NextTurn) != 0 || len(steered.NextStep) != 2 ||
		string(steered.NextStep[0]) != second || string(steered.NextStep[1]) != first {
		t.Fatalf("steer = %+v, %v", steered, err)
	}
	for _, data := range []string{
		`{"target":"next-turn","start":0,"inserted":[],"moveToNextStep":true}`,
		`{"target":"next-step","start":0,"removedCount":1,"inserted":[],"moveToNextStep":true}`,
		`{"target":"next-turn","start":2,"inserted":[]}`,
		`{"target":"next-turn","start":0,"removedCount":2,"inserted":[]}`,
		`{"target":"next-turn","start":0,"inserted":[{"id":"bad","role":"assistant","source":{"kind":"user"},"content":[]}]}`,
	} {
		if _, err := state.Next(v4InboxEvent(2, data)); err == nil {
			t.Fatalf("invalid inbox splice accepted: %s", data)
		}
	}
}

func TestLiveSessionInboxRejectsInvalidSpliceBeforeLogMutation(t *testing.T) {
	s, err := NewLiveSession(v4TestHeader("session"), &memoryV4Journal{})
	if err != nil {
		t.Fatal(err)
	}
	message := `{"id":"one","role":"user","source":{"kind":"user"},"content":[]}`
	input := EventInput{Type: "agent/inbox/spliced", Data: json.RawMessage(`{"target":"next-turn","start":0,"inserted":[` + message + `]}`)}
	if _, err := s.Append(input); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(input); err == nil {
		t.Fatal("duplicate inbox splice accepted")
	}
	if len(s.Events()) != 1 || len(s.Inbox().NextTurn) != 1 {
		t.Fatalf("rejected splice changed Session: events=%d inbox=%+v", len(s.Events()), s.Inbox())
	}
}

type memoryV4Journal struct{}

func (*memoryV4Journal) Append(_ context.Context, _ []Event) error { return nil }
func (*memoryV4Journal) Close() error                              { return nil }
