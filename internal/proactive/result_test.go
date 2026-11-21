package proactive

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go-bot/harness/agent"
	sessionformat "go-bot/harness/session"
	"go-bot/internal/modeldata"
)

const validResult = `{"workSummary":"Checked the available information.","nextCheck":{"at":"2026-11-22T09:30:00+08:00","reason":"Review the expected publication."},"notify":null}`

func TestResultRequiresNextCheckBeforeFinishing(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	for _, raw := range []string{
		strings.Replace(validResult, `"nextCheck":{"at":"2026-11-22T09:30:00+08:00","reason":"Review the expected publication."},`, "", 1),
		strings.Replace(validResult, `{"at":"2026-11-22T09:30:00+08:00","reason":"Review the expected publication."}`, "null", 1),
		strings.Replace(validResult, "2026-11-22T09:30:00+08:00", "2026-11-20T09:30:00+08:00", 1),
		strings.Replace(validResult, "2026-11-22T09:30:00+08:00", "2026-11-22T09:30:00", 1),
		strings.Replace(validResult, "Review the expected publication.", "  ", 1),
		strings.Replace(validResult, `,"notify":null`, "", 1),
		strings.Replace(validResult, `"notify":null`, `"notify":{"message":"Update","evidence":null}`, 1),
		strings.Replace(validResult, `"notify":null`, `"notify":{"message":"Update","evidence":[{"title":"Source"}]}`, 1),
	} {
		if _, err := DecodeResult(json.RawMessage(raw), now); err == nil {
			t.Fatalf("accepted invalid result: %s", raw)
		}
	}
	result, err := DecodeResult(json.RawMessage(validResult), now)
	if err != nil || result.Notify != nil || result.NextCheck.UnixMilli() != time.Date(2026, 11, 22, 1, 30, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("valid quiet decision: %+v, %v", result, err)
	}
	if _, err := DecodeResult(json.RawMessage(validResult), now.AddDate(0, 0, 10)); err == nil {
		t.Fatal("old time accepted as a new scheduling decision")
	}
	if _, err := DecodeResult(json.RawMessage(validResult), time.Time{}); err != nil {
		t.Fatalf("once-valid result cannot be recovered after its time passed: %v", err)
	}
}

type outputJournal struct {
	events []sessionformat.Event
	output *Output
	fail   bool
	t      *testing.T
}

func (j *outputJournal) Append(_ context.Context, events []sessionformat.Event) error {
	for _, event := range events {
		if event.Type == "tool/result" {
			if _, captured := j.output.Captured(); captured {
				j.t.Error("result became visible before its commit")
			}
			if j.fail {
				return errors.New("journal unavailable")
			}
		}
		j.events = append(j.events, event)
	}
	return nil
}
func (*outputJournal) Close() error { return nil }

type outputModel struct {
	calls   int
	correct bool
}

func (m *outputModel) Complete(_ context.Context, _ []agent.Message, _ []agent.ToolSpec) (modeldata.AssistantMessage, error) {
	m.calls++
	raw := strings.Replace(validResult, "2026-11-22T09:30:00+08:00", time.Now().Add(24*time.Hour).Format(time.RFC3339), 1)
	if m.correct && m.calls == 1 {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &fields)
		fields["nextCheck"] = json.RawMessage("null")
		encoded, _ := json.Marshal(fields)
		raw = string(encoded)
	}
	return modeldata.AssistantMessage{ToolCalls: []modeldata.ToolRequest{{ID: "finish-" + string(rune('a'+m.calls)), Name: "structured_output", Arguments: raw}}}, nil
}

func TestOutputUsesOrdinaryLoopCanCorrectAndCanRunAgain(t *testing.T) {
	output, err := NewOutput()
	if err != nil {
		t.Fatal(err)
	}
	journal := &outputJournal{output: output, t: t}
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "background-test", CreatedAt: 1, CWD: t.TempDir()}, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	model := &outputModel{correct: true}
	loop, err := agent.NewNativeLoop(s, model, "p", "m", "Complete with structured_output", nil, 4, 5)
	if err != nil {
		t.Fatal(err)
	}
	loop.SetToolSource(func(context.Context, string) ([]agent.Tool, error) { return []agent.Tool{output}, nil })
	if _, err := loop.RunTextTurn(t.Context(), "Check available information"); err != nil {
		t.Fatal(err)
	}
	if _, ok := output.Captured(); !ok || model.calls != 2 {
		t.Fatalf("bad decision should be corrected inside the same turn: captured=%v calls=%d", ok, model.calls)
	}
	turnEnds := 0
	for _, event := range s.DurableEvents() {
		if event.Type == "turn/end" {
			turnEnds++
		}
	}
	if turnEnds != 1 {
		t.Fatalf("correction created %d turns", turnEnds)
	}
	if results := CommittedResults(s.DurableEvents()); len(results) != 1 || results[0].Turn != 1 {
		t.Fatalf("recovery did not find the committed corrected decision: %+v", results)
	}
	output, err = NewOutput()
	if err != nil {
		t.Fatal(err)
	}
	journal.output = output
	if _, err := loop.RunTextTurn(t.Context(), "Check again"); err != nil {
		t.Fatal(err)
	}
	if _, ok := output.Captured(); !ok || model.calls != 3 {
		t.Fatalf("persistent Agent did not finish a fresh result: captured=%v calls=%d", ok, model.calls)
	}
	if err := sessionformat.ValidateV4Relationships(s.DurableEvents()); err != nil {
		t.Fatalf("ordinary journal relationships changed: %v", err)
	}
	if results := CommittedResults(s.DurableEvents()); len(results) != 2 || results[1].Turn != 2 {
		t.Fatalf("recovery lost separate per-turn results: %+v", results)
	}
}

func TestOutputDoesNotCaptureFailedCommit(t *testing.T) {
	output, err := NewOutput()
	if err != nil {
		t.Fatal(err)
	}
	journal := &outputJournal{output: output, fail: true, t: t}
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "background-failed", CreatedAt: 1, CWD: t.TempDir()}, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loop, err := agent.NewNativeLoop(s, &outputModel{}, "p", "m", "Complete with structured_output", []agent.Tool{output}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(t.Context(), "Check"); err == nil {
		t.Fatal("journal failure should fail the run")
	}
	if result, ok := output.Captured(); ok {
		t.Fatalf("failed result escaped: %+v", result)
	}
	if results := CommittedResults(s.DurableEvents()); len(results) != 0 {
		t.Fatalf("recovery fabricated a failed result: %+v", results)
	}
}
