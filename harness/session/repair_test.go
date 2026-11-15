package sessionformat

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func appendRepairTest(t *testing.T, s *LiveSession, eventType, data, op string, sources []uint64) Event {
	t.Helper()
	event, err := s.Append(EventInput{Type: eventType, Data: json.RawMessage(data), SurfaceOp: json.RawMessage(op), SourceEventSeqs: sources})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestRepairInterruptedV4TurnDistinguishesStartedAndNotStartedTools(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-started", true: "outcome-unknown"}[started], func(t *testing.T) {
			ctx := context.Background()
			s, err := NewLiveSession(v4TestHeader("session"), &memoryV4Journal{})
			if err != nil {
				t.Fatal(err)
			}
			appendRepairTest(t, s, "turn/start", `{"turn":1}`, "", nil)
			appendRepairTest(t, s, "step/start", `{"turn":1,"step":1}`, "", nil)
			appendRepairTest(t, s, "assistant/message", `{"turn":1,"step":1,"message":{"id":"a","role":"assistant","source":{"kind":"model","provider":"p","model":"m"},"content":[{"type":"tool-call","id":"c","name":"shell","arguments":"{}"}]},"stream":[]}`, `"append"`, nil)
			var call Event
			if started {
				call = appendRepairTest(t, s, "tool/call", `{"turn":1,"step":1,"callId":"c","name":"shell","arguments":"{}"}`, "", nil)
			}
			if err := s.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			lastTime := s.Events()[len(s.Events())-1].Time
			repaired, err := RepairInterruptedTurn(ctx, s)
			if err != nil || !repaired {
				t.Fatalf("repair = %v, %v", repaired, err)
			}
			events := s.DurableEvents()
			if len(events) < 3 || events[len(events)-3].Type != "tool/result" || events[len(events)-2].Type != "step/end" || events[len(events)-1].Type != "turn/end" {
				t.Fatalf("wrong repair order: %+v", events)
			}
			result := events[len(events)-3]
			wantCode := "TOOL_NOT_STARTED"
			if started {
				wantCode = "TOOL_OUTCOME_UNKNOWN"
				if string(result.SourceEventSeqs) != "["+strconv.FormatUint(call.Seq, 10)+"]" {
					t.Fatalf("started repair lost call source: %+v", result)
				}
			} else if len(result.SourceEventSeqs) != 0 {
				t.Fatalf("unstarted repair cited a call: %+v", result)
			}
			if !strings.Contains(string(result.Data), wantCode) || !strings.Contains(string(events[len(events)-1].Data), `"kind":"interrupted"`) {
				t.Fatalf("wrong repair facts: result=%s end=%s", result.Data, events[len(events)-1].Data)
			}
			for _, event := range events[len(events)-3:] {
				if event.Time != lastTime {
					t.Fatalf("synthetic event invented a later timestamp: %+v", event)
				}
			}
			if err := ValidateV4Relationships(events); err != nil {
				t.Fatalf("repaired log is not valid v4: %v", err)
			}
			if again, err := RepairInterruptedTurn(ctx, s); err != nil || again {
				t.Fatalf("repair repeated after closure: %v, %v", again, err)
			}
		})
	}
}
