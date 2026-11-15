package sessionformat

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeTodoSnapshotValidationAndProjection(t *testing.T) {
	for _, raw := range []string{
		`{"todos":null}`,
		`{"todos":[{"content":" ","status":"pending"}]}`,
		`{"todos":[{"content":"a","status":"pending"},{"content":" a ","status":"completed"}]}`,
		`{"todos":[{"content":"a","status":"unknown"}]}`,
		`{"todos":[{"content":"a","status":"pending","extra":true}]}`,
		`{"todos":[]} {"todos":[]}`,
	} {
		if _, err := DecodeTodoSnapshot(json.RawMessage(raw), true); err == nil {
			t.Errorf("accepted invalid native todo snapshot %s", raw)
		}
	}
	if _, err := DecodeTodoSnapshot(json.RawMessage(`{"todos":[{"content":"a","status":"in_progress"},{"content":"b","status":"in_progress"}]}`), false); err == nil {
		t.Fatal("single-active native todo policy accepted two active items")
	}
	canonical, err := DecodeTodoSnapshot(json.RawMessage(`{"todos":[{"content":" first ","status":"in_progress"},{"content":"second","status":"pending"}]}`), true)
	if err != nil || canonical.Todos[0].Content != "first" {
		t.Fatalf("native canonical todo = %+v, %v", canonical, err)
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	events := []Event{
		{Type: "todo/write", Seq: 0, Time: 1, Data: data},
		{Type: "turn/end", Seq: 1, Time: 2, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)},
	}
	if err := ValidateV4Rows(events); err != nil {
		t.Fatal(err)
	}
	todos, visible, err := ProjectTodos(events)
	if err != nil || !visible || len(todos) != 2 || todos[0].Content != "first" {
		t.Fatalf("native todo projection = %+v visible=%t err=%v", todos, visible, err)
	}
	events = append(events, Event{Type: "turn/start", Seq: 2, Time: 3, Data: json.RawMessage(`{"turn":2}`)})
	if _, visible, err := ProjectTodos(events); err != nil || visible {
		t.Fatalf("next turn kept old checklist: visible=%t err=%v", visible, err)
	}
	events = append(events, Event{Type: "todo/write", Seq: 3, Time: 4, Data: json.RawMessage(`{"todos":[]}`)})
	if empty, visible, err := ProjectTodos(events); err != nil || !visible || empty == nil || len(empty) != 0 {
		t.Fatalf("empty todo list was conflated with no list: %+v visible=%t err=%v", empty, visible, err)
	}
	invalid := Event{Type: "todo/write", Seq: 0, Time: 1, Data: json.RawMessage(`{"todos":[{"content":"bad","status":"invalid"}]}`)}
	if err := ValidateV4Rows([]Event{invalid}); err == nil || !strings.Contains(err.Error(), "todo") {
		t.Fatalf("invalid native todo row accepted: %v", err)
	}
}
