package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

type TodoSnapshot struct {
	Todos []TodoItem `json:"todos"`
}

type todoProjection struct {
	processed int
	current   []TodoItem
	visible   bool
	turn      uint64
	step      uint64
	err       error
}

// ValidateTodoSnapshot canonicalizes one whole-list replacement. The list is
// owned by this Session; there are no item IDs or per-item mutations.
func ValidateTodoSnapshot(snapshot TodoSnapshot, allowParallel bool) (TodoSnapshot, error) {
	if snapshot.Todos == nil {
		return TodoSnapshot{}, errors.New("todos must be an array")
	}
	seen := make(map[string]struct{}, len(snapshot.Todos))
	active := 0
	canonical := make([]TodoItem, len(snapshot.Todos))
	for i, item := range snapshot.Todos {
		item.Content = strings.TrimSpace(item.Content)
		if item.Content == "" {
			return TodoSnapshot{}, errors.New("invalid todo: `content` must be a non-empty string")
		}
		if _, exists := seen[item.Content]; exists {
			return TodoSnapshot{}, fmt.Errorf("invalid todos: duplicate content %q", item.Content)
		}
		seen[item.Content] = struct{}{}
		switch item.Status {
		case "pending", "completed":
		case "in_progress":
			active++
		default:
			return TodoSnapshot{}, fmt.Errorf("invalid todo status %q", item.Status)
		}
		canonical[i] = item
	}
	if !allowParallel && active > 1 {
		return TodoSnapshot{}, fmt.Errorf("invalid todos: at most one task may be in_progress (got %d)", active)
	}
	return TodoSnapshot{Todos: canonical}, nil
}

func DecodeTodoSnapshot(raw json.RawMessage, allowParallel bool) (TodoSnapshot, error) {
	return decodeTodoSnapshot(raw, allowParallel, false)
}

func decodeTodoSnapshot(raw json.RawMessage, allowParallel, requireCanonical bool) (TodoSnapshot, error) {
	var snapshot TodoSnapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return TodoSnapshot{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		// json.Decoder must consume exactly one value; a second value or a
		// malformed suffix is never a valid tool call/event payload.
		return TodoSnapshot{}, errors.New("todo snapshot must contain one JSON object")
	}
	canonical, err := ValidateTodoSnapshot(snapshot, allowParallel)
	if err != nil {
		return TodoSnapshot{}, err
	}
	if requireCanonical {
		for i, item := range snapshot.Todos {
			if item != canonical.Todos[i] {
				return TodoSnapshot{}, fmt.Errorf("todo %d was not stored in canonical form", i)
			}
		}
	}
	return canonical, nil
}

func (p *todoProjection) apply(event Event) error {
	switch event.Type {
	case "turn/start":
		p.current, p.visible = nil, false
		p.turn, p.step = event.Turn, 0
	case "step/start":
		if p.turn == event.Turn {
			p.step = event.Step
		}
	case "step/end":
		if p.turn == event.Turn && p.step == event.Step {
			p.step = 0
		}
	case "turn/end":
		if p.turn == event.Turn {
			p.turn, p.step = 0, 0
		}
	case "todo/write":
		snapshot, err := decodeTodoSnapshot(event.Data, true, true)
		if err != nil {
			return fmt.Errorf("invalid todo/write event %d: %w", event.Seq, err)
		}
		if event.Turn == 0 || event.Step == 0 || event.Turn != p.turn || event.Step != p.step {
			return fmt.Errorf("todo/write event %d is outside an active tool step", event.Seq)
		}
		p.current, p.visible = snapshot.Todos, true
	}
	return nil
}

func (s *Session) projectTodos() error {
	if s.todos.err != nil {
		return s.todos.err
	}
	for s.todos.processed < s.durable {
		if err := s.todos.apply(s.events[s.todos.processed]); err != nil {
			s.todos.err = err
			return err
		}
		s.todos.processed++
	}
	return nil
}

func (s *Session) ValidateTodos() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectTodos()
}

// Todos returns the latest whole-list state for the current turn. A new turn
// clears the presented list, while turn/end preserves the final checklist.
func (s *Session) Todos() ([]TodoItem, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.projectTodos(); err != nil {
		return nil, false, err
	}
	if !s.todos.visible {
		return nil, false, nil
	}
	return append([]TodoItem(nil), s.todos.current...), true, nil
}
