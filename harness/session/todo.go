package sessionformat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// TodoItem is the released todo/write payload, not deployment metadata.
// A write replaces the complete list owned by one Agent Session.
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

type TodoSnapshot struct {
	Todos []TodoItem `json:"todos"`
}

// DecodeTodoSnapshot accepts one strict tool argument object, trims content,
// and rejects duplicate or incoherent entries before they enter the v4 log.
func DecodeTodoSnapshot(raw json.RawMessage, allowParallel bool) (TodoSnapshot, error) {
	var snapshot TodoSnapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return TodoSnapshot{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return TodoSnapshot{}, errors.New("todo snapshot must contain one JSON object")
	}
	if snapshot.Todos == nil {
		return TodoSnapshot{}, errors.New("todos must be an array")
	}
	seen := make(map[string]bool, len(snapshot.Todos))
	active := 0
	for index := range snapshot.Todos {
		item := &snapshot.Todos[index]
		item.Content = strings.TrimSpace(item.Content)
		if item.Content == "" {
			return TodoSnapshot{}, errors.New("invalid todo: `content` must be a non-empty string")
		}
		if seen[item.Content] {
			return TodoSnapshot{}, fmt.Errorf("invalid todos: duplicate content %q", item.Content)
		}
		seen[item.Content] = true
		switch item.Status {
		case "pending", "completed":
		case "in_progress":
			active++
		default:
			return TodoSnapshot{}, fmt.Errorf("invalid todo status %q", item.Status)
		}
	}
	if !allowParallel && active > 1 {
		return TodoSnapshot{}, fmt.Errorf("invalid todos: at most one task may be in_progress (got %d)", active)
	}
	return snapshot, nil
}

// ProjectTodos is the original last-write-wins presentation rule. A new turn
// clears the previous checklist; turn/end leaves the finished one visible.
func ProjectTodos(events []Event) ([]TodoItem, bool, error) {
	var current []TodoItem
	visible := false
	for _, event := range events {
		switch event.Type {
		case "turn/start":
			current, visible = nil, false
		case "todo/write":
			snapshot, err := DecodeTodoSnapshot(event.Data, true)
			if err != nil {
				return nil, false, fmt.Errorf("todo/write at seq %d: %w", event.Seq, err)
			}
			current = make([]TodoItem, len(snapshot.Todos))
			copy(current, snapshot.Todos)
			visible = true
		}
	}
	return current, visible, nil
}
