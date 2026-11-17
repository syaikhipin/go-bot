package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-bot/harness/agent"
	"go-bot/harness/session"
	"go-bot/internal/testsupport/session"
)

type nativeTodoJournal struct{ events []sessionformat.Event }

func (j *nativeTodoJournal) Append(_ context.Context, events []sessionformat.Event) error {
	for _, event := range events {
		if event.Seq != uint64(len(j.events)) {
			return errors.New("native todo journal sequence mismatch")
		}
		j.events = append(j.events, event)
	}
	return nil
}
func (*nativeTodoJournal) Close() error { return nil }

type nativeTodoModel struct{ calls int }

func (m *nativeTodoModel) Complete(_ context.Context, _ []agent.Message, _ []agent.ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	if m.calls == 1 {
		return session.AssistantMessage{ToolCalls: []session.ToolRequest{{ID: "todo-1", Name: "todo_write", Arguments: `{"todos":[{"content":" first ","status":"in_progress"},{"content":"second","status":"pending"}]}`}}}, nil
	}
	return session.AssistantMessage{Content: "done"}, nil
}

func TestNativeTodoWritePersistsV4AndRestoresProjection(t *testing.T) {
	ctx := context.Background()
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-todos", CreatedAt: 1, CWD: "/workspace"}
	journal := &nativeTodoJournal{}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	tool := NativeTodoWrite{Session: live, AllowParallelInProgress: true}
	if err := tool.Validate(json.RawMessage(`{"todos":[{"content":"a","status":"pending"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"todos":[]}`)); err == nil || !strings.Contains(err.Error(), "active agent tool call") {
		t.Fatalf("native todo escaped owning tool call: %v", err)
	}
	model := &nativeTodoModel{}
	loop, err := agent.NewNativeLoop(live, model, "provider", "model", "prompt", []agent.Tool{tool}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := loop.RunTextTurn(ctx, "plan the work"); err != nil || answer != "done" {
		t.Fatalf("native todo turn = %q, %v", answer, err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := sessionformat.PhysicalArtifact{Header: header, Events: append([]sessionformat.Event(nil), journal.events...)}
	restored, err := sessionformat.RestoreLiveSession(artifact, &nativeTodoJournal{events: append([]sessionformat.Event(nil), journal.events...)})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	todos, visible, err := sessionformat.ProjectTodos(restored.DurableEvents())
	if err != nil || !visible || len(todos) != 2 || todos[0].Content != "first" || todos[0].Status != "in_progress" {
		t.Fatalf("restored native todos = %+v visible=%t err=%v", todos, visible, err)
	}
	var write, result bool
	for _, event := range artifact.Events {
		switch event.Type {
		case "todo/write":
			write = true
		case "tool/result":
			result = strings.Contains(string(event.Data), "Updated todo list: 1 pending, 1 in progress, 0 completed.")
		}
	}
	if !write || !result {
		t.Fatalf("native todo tool events missing write=%t result=%t", write, result)
	}
}
