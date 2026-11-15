package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func traceEvent(seq uint64, kind string, turn, step uint64, payload any) Event {
	data, _ := json.Marshal(payload)
	return Event{Seq: seq, Type: kind, Turn: turn, Step: step, At: time.Now().UTC(), Data: data}
}

func TestValidateExecutionTrace(t *testing.T) {
	complete := []Event{
		traceEvent(1, "turn/start", 1, 0, map[string]string{}),
		traceEvent(2, "step/start", 1, 1, map[string]string{}),
		traceEvent(3, "assistant/message", 1, 1, AssistantMessage{ToolCalls: []ToolRequest{{ID: "one", Name: "readfile"}}}),
		traceEvent(4, "tool/call", 1, 1, ToolCall{ID: "one", Name: "readfile"}),
		traceEvent(5, "tool/result", 1, 1, ToolResult{ID: "one", Name: "readfile", Content: "done"}),
		traceEvent(6, "step/end", 1, 1, map[string]string{}),
		traceEvent(7, "turn/end", 1, 0, map[string]string{"reason": "completed"}),
	}
	for _, events := range [][]Event{complete, complete[:3], complete[:4], complete[:5]} {
		if err := ValidateExecutionTrace(events); err != nil {
			t.Fatalf("valid complete/interrupted prefix: %v", err)
		}
	}
	copyEvents := func() []Event { return append([]Event(nil), complete...) }
	cases := []struct {
		name   string
		change func([]Event) []Event
		want   string
	}{
		{"out-of-order-step", func(events []Event) []Event { events[1].Step = 2; return events }, "step start is out of order"},
		{"orphan-tool-call", func(events []Event) []Event { events[2].Data = []byte(`{"content":"no tools"}`); return events }, "does not match"},
		{"changed-tool-arguments", func(events []Event) []Event {
			events[3].Data = []byte(`{"id":"one","name":"readfile","arguments":"{\"path\":\"other\"}"}`)
			return events
		}, "does not match"},
		{"wrong-tool-result", func(events []Event) []Event {
			events[4].Data = []byte(`{"id":"other","name":"readfile"}`)
			return events
		}, "pending call lifecycle"},
		{"missing-result", func(events []Event) []Event { return append(events[:4], events[5:]...) }, "noncontiguous sequence"},
		{"turn-end-while-step-open", func(events []Event) []Event { events[5] = traceEvent(6, "turn/end", 1, 0, nil); return events[:6] }, "current turn"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateExecutionTrace(test.change(copyEvents())); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
	missing := append([]Event(nil), complete[:4]...)
	missing = append(missing, traceEvent(5, "step/end", 1, 1, nil))
	if err := ValidateExecutionTrace(missing); err == nil || !strings.Contains(err.Error(), "lacking results") {
		t.Fatalf("closed step with unresolved tool call = %v", err)
	}
	notStarted := append([]Event(nil), complete[:3]...)
	notStarted = append(notStarted, traceEvent(4, "tool/result", 1, 1, ToolResult{ID: "one", Name: "readfile", IsError: true, Code: ToolNotStarted}))
	notStarted = append(notStarted, traceEvent(5, "step/end", 1, 1, nil), traceEvent(6, "turn/end", 1, 0, nil))
	if err := ValidateExecutionTrace(notStarted); err != nil {
		t.Fatalf("synthetic not-started result rejected: %v", err)
	}
}

func TestValidateExecutionTraceRejectsUnprojectableUserMessage(t *testing.T) {
	start := traceEvent(1, "turn/start", 1, 0, map[string]string{})
	for _, test := range []struct {
		name    string
		message Event
	}{
		{"scalar", traceEvent(2, "user/message", 1, 0, "text only")},
		{"missing role", traceEvent(2, "user/message", 1, 0, map[string]string{"content": "text"})},
		{"wrong role", traceEvent(2, "user/message", 1, 0, map[string]string{"role": "system", "content": "text"})},
		{"empty content", traceEvent(2, "user/message", 1, 0, map[string]string{"role": "user", "content": ""})},
		{"wrong turn", traceEvent(2, "user/message", 2, 0, map[string]string{"role": "user", "content": "text"})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateExecutionTrace([]Event{start, test.message}); err == nil || !strings.Contains(err.Error(), "user message") {
				t.Fatalf("unprojectable user message was accepted: %v", err)
			}
		})
	}
	valid := []Event{start, traceEvent(2, "user/message", 1, 0, map[string]string{"role": "user", "content": "text"})}
	if err := ValidateExecutionTrace(valid); err != nil {
		t.Fatalf("valid user message rejected: %v", err)
	}
}

func TestCompactionDoesNotErasePendingToolCalls(t *testing.T) {
	start := []Event{
		traceEvent(1, "turn/start", 1, 0, map[string]string{}),
		traceEvent(2, "step/start", 1, 1, struct{}{}),
		traceEvent(3, "assistant/message", 1, 1, AssistantMessage{ToolCalls: []ToolRequest{{ID: "write-1", Name: "writefile"}}}),
		traceEvent(4, "tool/call", 1, 1, ToolCall{ID: "write-1", Name: "writefile"}),
		traceEvent(5, "compaction/start", 1, 0, map[string]string{"id": "compact-1"}),
	}
	closedWithoutResult := append(append([]Event(nil), start...), traceEvent(6, "step/end", 1, 1, struct{}{}))
	if err := ValidateExecutionTrace(closedWithoutResult); err == nil || !strings.Contains(err.Error(), "lacking results") {
		t.Fatalf("compaction hid unfinished tool call: %v", err)
	}
	withResult := append(append([]Event(nil), start...),
		traceEvent(6, "tool/result", 1, 1, ToolResult{ID: "write-1", Name: "writefile", Content: "done"}),
		traceEvent(7, "step/end", 1, 1, struct{}{}),
		traceEvent(8, "turn/end", 1, 0, map[string]string{"reason": "completed"}),
	)
	if err := ValidateExecutionTrace(withResult); err != nil {
		t.Fatalf("settled tool call across compaction rejected: %v", err)
	}
	duplicateRequest := append(append([]Event(nil), start...),
		traceEvent(6, "tool/result", 1, 1, ToolResult{ID: "write-1", Name: "writefile", Content: "done"}),
		traceEvent(7, "assistant/message", 1, 1, AssistantMessage{ToolCalls: []ToolRequest{{ID: "write-1", Name: "writefile"}}}),
	)
	if err := ValidateExecutionTrace(duplicateRequest); err == nil || !strings.Contains(err.Error(), "duplicate assistant tool call") {
		t.Fatalf("compaction allowed a duplicate tool identity: %v", err)
	}
}

func TestValidateCancellationTraceRequiresMatchingPendingTurn(t *testing.T) {
	inbox := traceEvent(1, "user/inbox", 0, 0, map[string]string{"messageId": "m1", "turnId": "t1"})
	queued := []Event{inbox, traceEvent(2, "turn/cancel", 0, 0, map[string]string{"messageId": "m1", "turnId": "t1"})}
	if err := ValidateExecutionTrace(queued); err != nil {
		t.Fatalf("valid queued cancellation: %v", err)
	}
	running := []Event{
		inbox,
		traceEvent(2, "turn/start", 1, 0, map[string]string{"messageId": "m1", "turnId": "t1"}),
		traceEvent(3, "turn/cancel", 1, 0, map[string]string{"messageId": "m1", "turnId": "t1"}),
		traceEvent(4, "turn/end", 1, 0, map[string]string{"reason": "cancelled"}),
	}
	if err := ValidateExecutionTrace(running); err != nil {
		t.Fatalf("valid running cancellation: %v", err)
	}
	cases := []struct {
		name   string
		events []Event
	}{
		{"duplicate-turn-id", []Event{inbox, traceEvent(2, "user/inbox", 0, 0, map[string]string{"messageId": "m2", "turnId": "t1"})}},
		{"wrong-turn-id", []Event{inbox, traceEvent(2, "turn/cancel", 0, 0, map[string]string{"messageId": "m1", "turnId": "other"})}},
		{"duplicate", append(append([]Event(nil), queued...), traceEvent(3, "turn/cancel", 0, 0, map[string]string{"messageId": "m1", "turnId": "t1"}))},
		{"started-after-cancel", append(append([]Event(nil), queued...), traceEvent(3, "turn/start", 1, 0, map[string]string{"messageId": "m1", "turnId": "t1"}))},
		{"cancel-after-end", []Event{inbox, running[1], traceEvent(3, "turn/end", 1, 0, nil), traceEvent(4, "turn/cancel", 1, 0, map[string]string{"messageId": "m1", "turnId": "t1"})}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateExecutionTrace(test.events); err == nil {
				t.Fatal("invalid cancellation trace was accepted")
			}
		})
	}
}

func TestValidateInboxRequeueRequiresNoStepOrCancellation(t *testing.T) {
	inbox := traceEvent(1, "user/inbox", 0, 0, InboxRecord{MessageID: "m1", TurnID: "t1", IdempotencyKey: "k1", Content: "work"})
	start := traceEvent(2, "turn/start", 1, 0, map[string]string{"messageId": "m1", "turnId": "t1"})
	requeue := traceEvent(3, "turn/end", 1, 0, map[string]string{"reason": TurnInterruptedBeforeStep})
	valid := []Event{inbox, start, requeue, traceEvent(4, "turn/start", 2, 0, map[string]string{"messageId": "m1", "turnId": "t1"})}
	if err := ValidateExecutionTrace(valid); err != nil {
		t.Fatalf("safe requeue rejected: %v", err)
	}
	admitted := []Event{
		inbox, start,
		traceEvent(3, "user/message", 1, 0, map[string]string{"role": "user", "content": "work"}),
		traceEvent(4, "turn/end", 1, 0, map[string]string{"reason": TurnInterruptedBeforeStep}),
		traceEvent(5, "turn/start", 2, 0, map[string]string{"messageId": "m1", "turnId": "t1"}),
	}
	if err := ValidateExecutionTrace(admitted); err != nil {
		t.Fatalf("safe requeue after user admission rejected: %v", err)
	}
	for name, events := range map[string][]Event{
		"already-entered-step":    {inbox, start, traceEvent(3, "step/start", 1, 1, struct{}{}), traceEvent(4, "step/end", 1, 1, struct{}{}), traceEvent(5, "turn/end", 1, 0, map[string]string{"reason": TurnInterruptedBeforeStep})},
		"auxiliary-model-started": {inbox, start, traceEvent(3, "compaction/start", 1, 0, map[string]string{"id": "c1"}), traceEvent(4, "turn/end", 1, 0, map[string]string{"reason": TurnInterruptedBeforeStep})},
		"cancelled":               {inbox, start, traceEvent(3, "turn/cancel", 1, 0, map[string]string{"messageId": "m1", "turnId": "t1"}), traceEvent(4, "turn/end", 1, 0, map[string]string{"reason": TurnInterruptedBeforeStep})},
		"no-inbox":                {traceEvent(1, "turn/start", 1, 0, struct{}{}), traceEvent(2, "turn/end", 1, 0, map[string]string{"reason": TurnInterruptedBeforeStep})},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateExecutionTrace(events); err == nil {
				t.Fatal("unsafe inbox requeue was accepted")
			}
		})
	}
}
