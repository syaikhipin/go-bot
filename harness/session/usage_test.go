package sessionformat

import (
	"encoding/json"
	"math"
	"testing"
)

func TestProjectUsageCountsDurableV4OutcomesWithoutCredits(t *testing.T) {
	events := []Event{
		{Type: "step/start", Seq: 0, Data: json.RawMessage(`{"turn":1,"step":1}`)},
		{Type: "assistant/attempt", Seq: 1, Data: json.RawMessage(`{"turn":1,"step":1,"stream":[{"type":"chunk","chunk":{"type":"usage","usage":{"inputTokens":2,"outputTokens":1}}}]}`)},
		{Type: "assistant/message", Seq: 2, Data: json.RawMessage(`{"turn":1,"step":1,"usage":{"inputTokens":3,"outputTokens":4,"cacheReadTokens":5,"cacheWriteTokens":1}}`)},
		{Type: "step/start", Seq: 3, Data: json.RawMessage(`{"turn":1,"step":2}`)},
		{Type: "assistant/attempt", Seq: 4, Data: json.RawMessage(`{"turn":1,"step":2,"stream":[]}`)},
		{Type: "compaction/summary", Seq: 5, Data: json.RawMessage(`{"usage":{"inputTokens":7,"outputTokens":2}}`)},
		{Type: "compaction/end", Seq: 6, Data: json.RawMessage(`{"error":{"message":"summary failed"}}`)},
	}
	got, err := ProjectUsage(events)
	if err != nil {
		t.Fatal(err)
	}
	if got.StepsStarted != 2 || got.AssistantAttempts != 2 || got.AssistantMessages != 1 || got.SummarySucceeded != 1 || got.SummaryFailed != 1 || got.ReportedSamples != 3 || got.UnmeteredOutcomes != 1 || got.InputTokens != 12 || got.OutputTokens != 7 || got.CacheReadTokens != 5 || got.CacheWriteTokens != 1 || got.TotalTokens != 25 {
		t.Fatalf("v4 telemetry = %+v", got)
	}
}

func TestProjectUsageRejectsOverflow(t *testing.T) {
	events := []Event{{Type: "assistant/message", Data: json.RawMessage(`{"usage":{"inputTokens":9223372036854775807,"outputTokens":1}}`)}}
	if _, err := ProjectUsage(events); err == nil {
		t.Fatal("overflowing v4 usage was accepted")
	}
	snapshot := UsageSnapshot{TotalTokens: math.MaxInt64}
	if err := snapshot.addUsage(v4Usage{OutputTokens: 1}); err == nil {
		t.Fatal("overflowing accumulated v4 usage was accepted")
	}
}
