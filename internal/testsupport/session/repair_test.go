package session

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRepairBeforeStepRequeuesDurableInbox(t *testing.T) {
	ctx := context.Background()
	store := FileStore{Root: t.TempDir(), Claim: (&ProcessClaims{}).Acquire}
	header := testHeader()
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	input := InboxRecord{MessageID: "message-1", TurnID: "turn-1", IdempotencyKey: "key-1", Content: "do the work"}
	for _, event := range []struct {
		kind string
		turn uint64
		data any
	}{
		{"user/inbox", 0, input},
		{"turn/start", 1, map[string]string{"messageId": input.MessageID, "turnId": input.TurnID}},
	} {
		if _, err := s.Append(event.kind, event.turn, 0, event.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	opened, restoredHeader, events, err := store.Open(ctx, header.UserID, header.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateExecutionTrace(events); err != nil {
		t.Fatal(err)
	}
	restored, err := Resume(restoredHeader, events, opened)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if repaired, err := RepairInterrupted(ctx, restored); err != nil || !repaired {
		t.Fatalf("repair = %t, %v", repaired, err)
	}
	if err := ValidateExecutionTrace(restored.DurableEvents()); err != nil {
		t.Fatalf("requeued trace is invalid: %v", err)
	}
	if state, found, err := restored.InboxByKey(input.IdempotencyKey); err != nil || !found || state.Finished || state.StartedTurn != 0 {
		t.Fatalf("accepted message was consumed: %#v found=%t err=%v", state, found, err)
	}
	if activity, err := restored.DurableActivity(); err != nil || activity.Idle {
		t.Fatalf("requeued message looked idle: %#v %v", activity, err)
	}
	if next, turn, found, err := restored.NextInbox(); err != nil || !found || next != input || turn != 2 {
		t.Fatalf("requeued input = %#v turn=%d found=%t err=%v", next, turn, found, err)
	}
}

func TestRepairBeforeStepDoesNotRequeueCancelledInbox(t *testing.T) {
	s, err := New(appendStoreHeader(), activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	input := InboxRecord{MessageID: "m1", TurnID: "t1", IdempotencyKey: "k1", Content: "work"}
	for _, event := range []struct {
		kind string
		turn uint64
		data any
	}{
		{"user/inbox", 0, input},
		{"turn/start", 1, map[string]string{"messageId": input.MessageID, "turnId": input.TurnID}},
		{"turn/cancel", 1, map[string]string{"messageId": input.MessageID, "turnId": input.TurnID}},
	} {
		if _, err := s.Append(event.kind, event.turn, 0, event.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repaired, err := RepairInterrupted(context.Background(), s); err != nil || !repaired {
		t.Fatalf("cancelled repair = %t, %v", repaired, err)
	}
	if _, _, found, err := s.NextInbox(); err != nil || found {
		t.Fatalf("cancelled input was replayed: found=%t err=%v", found, err)
	}
	var end struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(s.DurableEvents()[3].Data, &end); err != nil || end.Reason != "interrupted" {
		t.Fatalf("cancelled turn end = %#v %v", end, err)
	}
}

func TestRepairInterruptedToolOutcomeUnknown(t *testing.T) {
	ctx := context.Background()
	store := FileStore{Root: t.TempDir(), Claim: (&ProcessClaims{}).Acquire}
	header := testHeader()
	j, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(header, j)
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		typeName string
		step     uint64
		data     any
	}{
		{"turn/start", 0, nil},
		{"step/start", 1, nil},
		{"assistant/message", 1, AssistantMessage{ToolCalls: []ToolRequest{{ID: "c1", Name: "writefile"}, {ID: "c2", Name: "readfile"}}}},
		{"tool/call", 1, ToolCall{ID: "c1", Name: "writefile"}},
	}
	for _, step := range steps {
		if _, err := s.Append(step.typeName, 1, step.step, step.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	j2, h2, events, err := store.Open(ctx, header.UserID, header.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := Resume(h2, events, j2)
	if err != nil {
		t.Fatal(err)
	}
	repaired, err := RepairInterrupted(ctx, resumed)
	if err != nil || !repaired {
		t.Fatalf("repair = %v, %v", repaired, err)
	}
	got := resumed.Events()
	if len(got) != 8 {
		t.Fatalf("event count = %d, want 8", len(got))
	}
	var first, second ToolResult
	if err := json.Unmarshal(got[4].Data, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got[5].Data, &second); err != nil {
		t.Fatal(err)
	}
	if first.Code != ToolOutcomeUnknown || second.Code != ToolNotStarted || got[6].Type != "step/end" || got[7].Type != "turn/end" {
		t.Fatalf("synthetic tail = %+v %+v %+v %+v", first, second, got[6], got[7])
	}
	if again, err := RepairInterrupted(ctx, resumed); err != nil || again {
		t.Fatalf("second repair = %v, %v", again, err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
}
