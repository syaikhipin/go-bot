package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"go-bot/harness/session"
	"go-bot/internal/modeldata"
)

type nativeRepeatModel struct {
	calls    int
	toolRuns int
	seen     [][]Message
}

func (m *nativeRepeatModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (modeldata.AssistantMessage, error) {
	m.calls++
	m.seen = append(m.seen, append([]Message(nil), messages...))
	if m.calls > m.toolRuns {
		return modeldata.AssistantMessage{Content: "done"}, nil
	}
	arguments := `{"nested":{"b":2,"a":1},"q":"same"}`
	if m.calls%2 == 0 {
		arguments = `{"q":"same","nested":{"a":1,"b":2}}`
	}
	return modeldata.AssistantMessage{ToolCalls: []modeldata.ToolRequest{{
		ID: fmt.Sprintf("call-%d", m.calls), Name: "echo", Arguments: arguments,
	}}}, nil
}

func TestNativeRepeatReminderUsesDurableV4Context(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-repeat", CreatedAt: 1}
	journal := &nativeTestJournal{}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model, tool := &nativeRepeatModel{toolRuns: 5}, &nativeTestTool{}
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", []Tool{tool}, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := loop.RunTextTurn(context.Background(), "use echo"); err != nil || answer != "done" || tool.calls != 5 {
		t.Fatalf("native repeated turn: answer=%q err=%v calls=%d", answer, err, tool.calls)
	}
	var notices []struct {
		Source struct {
			Kind    string `json:"kind"`
			Form    string `json:"form"`
			Summary string `json:"summary"`
		} `json:"source"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	var resultStep, noticeStep int
	for _, event := range live.DurableEvents() {
		if event.Type == "tool/result" {
			resultStep++
		}
		if event.Type != "user/message" {
			continue
		}
		var message struct {
			Source struct {
				Kind    string `json:"kind"`
				Form    string `json:"form"`
				Summary string `json:"summary"`
			} `json:"source"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(event.Data, &message); err != nil {
			t.Fatal(err)
		}
		if message.Source.Kind == "repeat-tool-reminder" {
			notices = append(notices, message)
			if resultStep != 3+2*noticeStep {
				t.Fatalf("reminder appeared after %d results, want %d", resultStep, 3+2*noticeStep)
			}
			noticeStep++
		}
	}
	if len(notices) != 2 || notices[0].Source.Form != "notice" || notices[0].Source.Summary != "echo × 3" || notices[1].Source.Summary != "echo × 5" ||
		!strings.Contains(notices[0].Content[0].Text, "repeating the exact same tool call") ||
		!strings.Contains(notices[1].Content[0].Text, "consecutive_calls: 5") {
		t.Fatalf("wrong native reminder events: %+v", notices)
	}
	var fourthSawReminder bool
	for _, message := range model.seen[3] {
		fourthSawReminder = fourthSawReminder || message.Role == "user" && strings.Contains(message.Content, "repeating the exact same tool call")
	}
	if !fourthSawReminder {
		t.Fatal("fourth model request did not see the third-call reminder")
	}
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("SERVERAGENTS_V4_REPEAT_REMINDER_EXPORT_PATH"); path != "" {
		if err := os.WriteFile(path, nativeTestJournalJSONL(t, header, journal), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

type nativeRepeatAcrossTurnsModel struct{ calls int }

func (m *nativeRepeatAcrossTurnsModel) Complete(_ context.Context, _ []Message, _ []ToolSpec) (modeldata.AssistantMessage, error) {
	m.calls++
	switch m.calls {
	case 1, 2, 4:
		return modeldata.AssistantMessage{ToolCalls: []modeldata.ToolRequest{{
			ID: fmt.Sprintf("turn-call-%d", m.calls), Name: "echo", Arguments: `{}`,
		}}}, nil
	default:
		return modeldata.AssistantMessage{Content: "done"}, nil
	}
}

func TestNativeRepeatReminderResetsOnNewUserTurn(t *testing.T) {
	live, err := sessionformat.NewLiveSession(
		sessionformat.Header{Type: "session", Version: 4, ID: "native-repeat-reset", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model, tool := &nativeRepeatAcrossTurnsModel{}, &nativeTestTool{}
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", []Tool{tool}, 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"first request", "new request"} {
		if answer, err := loop.RunTextTurn(context.Background(), prompt); err != nil || answer != "done" {
			t.Fatalf("%q: answer=%q err=%v", prompt, answer, err)
		}
	}
	if tool.calls != 3 {
		t.Fatalf("executed %d tool calls, want 3", tool.calls)
	}
	for _, event := range live.DurableEvents() {
		if event.Type == "user/message" && strings.Contains(string(event.Data), `"repeat-tool-reminder"`) {
			t.Fatal("the new direct user message did not reset the repeat chain")
		}
	}
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
}

type nativeParallelRepeatModel struct {
	calls int
	seen  [][]Message
}

func (m *nativeParallelRepeatModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (modeldata.AssistantMessage, error) {
	m.calls++
	m.seen = append(m.seen, append([]Message(nil), messages...))
	if m.calls > 1 {
		return modeldata.AssistantMessage{Content: "done"}, nil
	}
	return modeldata.AssistantMessage{ToolCalls: []modeldata.ToolRequest{
		{ID: "parallel-1", Name: "parallel_echo", Arguments: `{}`},
		{ID: "parallel-2", Name: "parallel_echo", Arguments: `{}`},
		{ID: "parallel-3", Name: "parallel_echo", Arguments: `{}`},
	}}, nil
}

type nativeParallelRepeatTool struct {
	releaseFirst chan struct{}
	calls        atomic.Int32
}

func (*nativeParallelRepeatTool) Spec() ToolSpec {
	return ToolSpec{Name: "parallel_echo", Description: "Echo in parallel", Parameters: json.RawMessage(`{"type":"object"}`), ParallelSafe: true}
}

func (*nativeParallelRepeatTool) Validate(raw json.RawMessage) error {
	var value map[string]any
	return json.Unmarshal(raw, &value)
}

func (t *nativeParallelRepeatTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	id, _ := ToolCallIDFromContext(ctx)
	t.calls.Add(1)
	if id == "parallel-1" {
		select {
		case <-t.releaseFirst:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	} else if id == "parallel-3" {
		close(t.releaseFirst)
	}
	return id, nil
}

func TestNativeRepeatReminderAfterParallelStepResults(t *testing.T) {
	live, err := sessionformat.NewLiveSession(
		sessionformat.Header{Type: "session", Version: 4, ID: "native-repeat-parallel", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeParallelRepeatModel{}
	tool := &nativeParallelRepeatTool{releaseFirst: make(chan struct{})}
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", []Tool{tool}, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := loop.RunTextTurn(context.Background(), "parallel calls"); err != nil || answer != "done" || tool.calls.Load() != 3 {
		t.Fatalf("parallel repeated turn: answer=%q err=%v calls=%d", answer, err, tool.calls.Load())
	}
	results, reminder, stepEnd := 0, -1, -1
	for index, event := range live.DurableEvents() {
		switch event.Type {
		case "tool/result":
			results++
		case "user/message":
			if strings.Contains(string(event.Data), `"repeat-tool-reminder"`) {
				reminder = index
				if results != 3 {
					t.Fatalf("parallel reminder preceded tool results: %d", results)
				}
			}
		case "step/end":
			if reminder >= 0 && stepEnd < 0 {
				stepEnd = index
			}
		}
	}
	if reminder < 0 || stepEnd <= reminder || len(model.seen) != 2 {
		t.Fatalf("parallel reminder was not persisted between tool results and step end: reminder=%d stepEnd=%d", reminder, stepEnd)
	}
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
}

type nativeBudgetRepeatModel struct{}

func (nativeBudgetRepeatModel) Complete(context.Context, []Message, []ToolSpec) (modeldata.AssistantMessage, error) {
	return modeldata.AssistantMessage{ToolCalls: []modeldata.ToolRequest{
		{ID: "budget-1", Name: "echo", Arguments: `{}`},
		{ID: "budget-2", Name: "echo", Arguments: `{}`},
		{ID: "budget-3", Name: "echo", Arguments: `{}`},
		{ID: "budget-4", Name: "echo", Arguments: `{}`},
	}}, nil
}

func TestNativeRepeatReminderDoesNotCountUndispatchedBudgetCalls(t *testing.T) {
	live, err := sessionformat.NewLiveSession(
		sessionformat.Header{Type: "session", Version: 4, ID: "native-repeat-budget", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	tool := &nativeTestTool{}
	loop, err := NewNativeLoop(live, nativeBudgetRepeatModel{}, "provider", "model", "prompt", []Tool{tool}, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "run echo"); err == nil || !strings.Contains(err.Error(), "tool-call budget") {
		t.Fatalf("expected tool budget error, got %v", err)
	}
	if tool.calls != 2 {
		t.Fatalf("executed %d calls, want 2", tool.calls)
	}
	var budgeted int
	for _, event := range live.DurableEvents() {
		if event.Type == "tool/result" && strings.Contains(string(event.Data), "TOOL_BUDGET_EXCEEDED") {
			budgeted++
		}
		if event.Type == "user/message" && strings.Contains(string(event.Data), `"repeat-tool-reminder"`) {
			t.Fatal("budget-rejected calls advanced the repeat reminder chain")
		}
	}
	if budgeted != 2 {
		t.Fatalf("found %d budgeted results, want 2", budgeted)
	}
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatReminderCanonicalJSONAndUTF16Preview(t *testing.T) {
	if got := canonicalToolArguments(`{"q":"<&>"}`); got != `{"q":"<&>"}` {
		t.Fatalf("canonical JSON escaped HTML unlike JSON.stringify: %q", got)
	}
	guard, err := newRepeatToolReminder(&RepeatReminderConfig{Thresholds: []int{2, 3}, ArgumentsPreviewChars: 10})
	if err != nil {
		t.Fatal(err)
	}
	call := modeldata.ToolRequest{Name: "echo", Arguments: `{"q":"😀😀😀"}`}
	if reminder := guard.observe(call); reminder != nil {
		t.Fatalf("first call produced reminder: %+v", reminder)
	}
	if reminder := guard.observe(call); reminder == nil || !strings.Contains(reminder.Content, "repeating the exact same tool call") {
		t.Fatalf("second call missed gentle reminder: %+v", reminder)
	}
	if reminder := guard.observe(call); reminder == nil || !strings.Contains(reminder.Content, `- arguments: {"q":"😀😀… (+4 more chars)`) {
		t.Fatalf("UTF-16 preview differs from upstream string slice: %+v", reminder)
	}
}

type nativeCancelRepeatModel struct{}

func (nativeCancelRepeatModel) Complete(context.Context, []Message, []ToolSpec) (modeldata.AssistantMessage, error) {
	return modeldata.AssistantMessage{ToolCalls: []modeldata.ToolRequest{
		{ID: "cancel-1", Name: "cancel_echo", Arguments: `{}`},
		{ID: "cancel-2", Name: "cancel_echo", Arguments: `{}`},
		{ID: "cancel-3", Name: "cancel_echo", Arguments: `{}`},
	}}, nil
}

type nativeCancelRepeatTool struct {
	cancel context.CancelFunc
	calls  int
}

func (*nativeCancelRepeatTool) Spec() ToolSpec {
	return ToolSpec{Name: "cancel_echo", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (*nativeCancelRepeatTool) Validate(json.RawMessage) error { return nil }

func (tool *nativeCancelRepeatTool) Execute(context.Context, json.RawMessage) (string, error) {
	tool.calls++
	tool.cancel()
	return "cancelled", nil
}

func TestNativeRepeatReminderDoesNotCountCancelledUndispatchedCalls(t *testing.T) {
	live, err := sessionformat.NewLiveSession(
		sessionformat.Header{Type: "session", Version: 4, ID: "native-repeat-cancel", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tool := &nativeCancelRepeatTool{cancel: cancel}
	loop, err := NewNativeLoop(live, nativeCancelRepeatModel{}, "provider", "model", "prompt", []Tool{tool}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(ctx, "run echo"); err == nil {
		t.Fatal("cancelled turn returned success")
	}
	if tool.calls != 1 || loop.repeatGuard.count != 1 {
		t.Fatalf("cancelled calls advanced reminder: executed=%d count=%d", tool.calls, loop.repeatGuard.count)
	}
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
}
