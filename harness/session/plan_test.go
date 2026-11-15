package sessionformat

import (
	"encoding/json"
	"testing"
)

func TestNativePlanFoldTracksLoggedModeAndCommandSelection(t *testing.T) {
	events := []Event{
		{Type: "command/run", Seq: 0, Data: json.RawMessage(`{"commandId":"c1","name":"plan","args":""}`)},
	}
	state, err := FoldPlan(events)
	if err != nil || state.Active || !state.Pending || state.Wanted == nil || !*state.Wanted {
		t.Fatalf("running plan entry = %+v, %v", state, err)
	}
	events = append(events, Event{Type: "command/done", Seq: 1, Data: json.RawMessage(`{"commandId":"c1","kind":"success"}`)})
	state, err = FoldPlan(events)
	if err != nil || state.Active || !state.Pending || state.Wanted == nil || !*state.Wanted {
		t.Fatalf("pending plan entry = %+v, %v", state, err)
	}
	events = append(events,
		Event{Type: "plan/mode", Seq: 2, Data: json.RawMessage(`{"active":true}`)},
		Event{Type: "request/header", Seq: 3, Data: json.RawMessage(`{"header":{}}`)},
	)
	state, err = FoldPlan(events)
	if err != nil || !state.Active || state.Pending || state.Wanted != nil || state.ActiveAtLastHeader == nil || !*state.ActiveAtLastHeader {
		t.Fatalf("committed plan entry = %+v, %v", state, err)
	}
	events = append(events,
		Event{Type: "command/run", Seq: 4, Data: json.RawMessage(`{"commandId":"c2","name":"plan","args":"off"}`)},
		Event{Type: "command/done", Seq: 5, Data: json.RawMessage(`{"commandId":"c2","kind":"success"}`)},
	)
	state, err = FoldPlan(events)
	if err != nil || !state.Active || !state.Pending || state.Wanted == nil || *state.Wanted {
		t.Fatalf("pending plan exit = %+v, %v", state, err)
	}
	events = append(events, Event{Type: "plan/mode", Seq: 6, Data: json.RawMessage(`{"active":false}`)})
	state, err = FoldPlan(events)
	if err != nil || state.Active || state.Pending || state.Wanted != nil {
		t.Fatalf("committed plan exit = %+v, %v", state, err)
	}
}

func TestNativePlanModeRejectsNoncanonicalPayload(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `{"active":null}`, `{"active":"true"}`,
		`{"active":true,"version":1}`, `{"active":1}`,
	} {
		if _, err := DecodePlanMode(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted non-v4 plan/mode %s", raw)
		}
	}
	if _, err := DecodePlanMode(json.RawMessage(`{"active":false}`)); err != nil {
		t.Fatal(err)
	}
}
