package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/session"
)

// NativeTodoWrite writes the released todo/write event to the owning v4
// Session. It does not adapt or read the development-era Session format.
type NativeTodoWrite struct {
	Session                 *sessionformat.LiveSession
	AllowParallelInProgress bool
}

func (t NativeTodoWrite) Spec() agent.ToolSpec {
	description := "Record and update a task list to plan multi-step work and show progress; skip it for trivial single-step tasks. Add one todo per concrete step before you start. "
	if t.AllowParallelInProgress {
		description += "While work remains, keep the todos being worked on `in_progress`, several only when work runs in parallel. "
	} else {
		description += "While work remains, keep exactly one todo `in_progress`. "
	}
	description += "Mark each todo `completed` as soon as it is done."
	return agent.ToolSpec{
		Name: "todo_write", Description: description,
		Parameters: json.RawMessage(`{"type":"object","properties":{"todos":{"type":"array","description":"The COMPLETE task list, replacing any previous list.","items":{"type":"object","properties":{"content":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed"]}},"required":["content","status"],"additionalProperties":false}}},"required":["todos"]}`),
	}
}

func decodeNativeTodoArgs(raw json.RawMessage, allowParallel bool) (sessionformat.TodoSnapshot, error) {
	projected, _, err := projectToolRoot(raw, "todos")
	if err != nil {
		return sessionformat.TodoSnapshot{}, err
	}
	return sessionformat.DecodeTodoSnapshot(projected, allowParallel)
}

func (t NativeTodoWrite) Validate(raw json.RawMessage) error {
	if t.Session == nil {
		return errors.New("todo_write requires an owning agent session")
	}
	_, err := decodeNativeTodoArgs(raw, t.AllowParallelInProgress)
	return err
}

func (t NativeTodoWrite) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if t.Session == nil {
		return "", errors.New("todo_write requires an owning agent session")
	}
	if _, _, ok := agent.ToolPositionFromContext(ctx); !ok {
		return "", errors.New("todo_write requires an active agent tool call")
	}
	snapshot, err := decodeNativeTodoArgs(raw, t.AllowParallelInProgress)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	if _, err := t.Session.Append(sessionformat.EventInput{Type: "todo/write", Data: data}); err != nil {
		return "", err
	}
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := t.Session.Flush(commitCtx); err != nil {
		return "", err
	}
	pending, active, completed := 0, 0, 0
	for _, item := range snapshot.Todos {
		switch item.Status {
		case "pending":
			pending++
		case "in_progress":
			active++
		case "completed":
			completed++
		}
	}
	return fmt.Sprintf("Updated todo list: %d pending, %d in progress, %d completed.", pending, active, completed), nil
}
