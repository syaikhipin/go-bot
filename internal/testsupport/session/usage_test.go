package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUsageProjectionFollowsOnlyDurablePrefixAndReplays(t *testing.T) {
	ctx := context.Background()
	header := Header{SchemaVersion: SchemaVersion, UserID: "alice", SessionID: "usage", Mode: "container", Workspace: "/workspace", CreatedAt: time.Now().UTC()}
	store := FileStore{Root: t.TempDir(), Claim: (&ProcessClaims{}).Acquire}
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	record, err := New(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(kind string, turn, step uint64, data any) {
		t.Helper()
		if _, err := record.Append(kind, turn, step, data); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent("turn/start", 1, 0, map[string]string{})
	appendEvent("step/start", 1, 1, map[string]string{})
	appendEvent("model/request", 1, 1, map[string]string{"attemptId": "a1"})
	appendEvent("assistant/message", 1, 1, AssistantMessage{Content: "answer", AttemptID: "a1", Usage: &ModelUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120, CachedInputTokens: 60}})
	appendEvent("step/end", 1, 1, map[string]string{})
	appendEvent("turn/end", 1, 0, map[string]string{"reason": "completed"})
	if zero, err := record.DurableUsage(); err != nil || zero.MainRequestCheckpoints != 0 {
		t.Fatalf("unflushed usage leaked: %#v, %v", zero, err)
	}
	if err := record.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := record.DurableUsage()
	if err != nil || first.MainRequestCheckpoints != 1 || first.MainSucceeded != 1 || first.ReportedSamples != 1 || first.InputTokens != 100 || first.TotalTokens != 120 || first.CachedInputTokens != 60 {
		t.Fatalf("first durable usage = %#v, %v", first, err)
	}
	appendEvent("turn/start", 2, 0, map[string]string{})
	appendEvent("step/start", 2, 1, map[string]string{})
	appendEvent("model/request", 2, 1, map[string]string{"attemptId": "a2"})
	appendEvent("model/failure", 2, 1, map[string]any{"attemptId": "a2", "reason": "error", "usage": ModelUsage{InputTokens: 50, TotalTokens: 50}})
	appendEvent("step/end", 2, 1, map[string]string{})
	appendEvent("turn/end", 2, 0, map[string]string{"reason": "model_error"})
	if err := record.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	live, err := record.DurableUsage()
	if err != nil {
		t.Fatal(err)
	}
	want := UsageSnapshot{
		MainRequestCheckpoints: 2, MainSucceeded: 1, MainFailed: 1,
		ReportedSamples: 2, InputTokens: 150, OutputTokens: 20,
		TotalTokens: 170, CachedInputTokens: 60,
	}
	if live != want {
		t.Fatalf("incremental usage = %#v, want %#v", live, want)
	}
	if rebuilt, err := ProjectUsage(record.DurableEvents()); err != nil || rebuilt != want {
		t.Fatalf("replayed usage = %#v, %v", rebuilt, err)
	}
	if err := record.Close(); err != nil {
		t.Fatal(err)
	}
	reader, restoredHeader, events, err := store.Open(ctx, header.UserID, header.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Resume(restoredHeader, events, reader)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if resumed, err := restored.DurableUsage(); err != nil || resumed != want {
		t.Fatalf("resumed usage = %#v, %v", resumed, err)
	}
}

func TestUsageProjectionRejectsDuplicateAttemptAndInvalidCount(t *testing.T) {
	clock := time.Now().UTC()
	event := func(seq uint64, kind string, data string) Event {
		return Event{Seq: seq, Type: kind, At: clock, Data: json.RawMessage(data)}
	}
	for _, sample := range []struct {
		name   string
		events []Event
		want   string
	}{
		{"duplicate-attempt", []Event{event(1, "model/request", `{"attemptId":"a1"}`), event(2, "model/failure", `{"attemptId":"a1"}`), event(3, "model/request", `{"attemptId":"a1"}`)}, "duplicate"},
		{"unknown-outcome", []Event{event(1, "model/failure", `{"attemptId":"missing"}`)}, "no matching"},
		{"negative-usage", []Event{event(1, "assistant/message", `{"usage":{"inputTokens":-1}}`)}, "invalid"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			if _, err := ProjectUsage(sample.events); err == nil || !strings.Contains(err.Error(), sample.want) {
				t.Fatalf("usage projection error = %v, want %q", err, sample.want)
			}
		})
	}
}
