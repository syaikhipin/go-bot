package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"go-bot/harness/session"
	"go-bot/internal/testsupport/session"
)

type scheduledNativeModel struct {
	calls []session.ToolRequest
	steps int
}

// Err changes between the scheduler's pre-flush check and LiveSession.Flush.
// A real cancellation can land in exactly this gap.
type cancelBetweenToolIntentChecks struct {
	context.Context
	checks int
}

func (c *cancelBetweenToolIntentChecks) Err() error {
	c.checks++
	if c.checks > 1 {
		return context.Canceled
	}
	return nil
}

func TestNativeToolIntentCheckpointSurvivesCancellationRace(t *testing.T) {
	loop, s := scheduledNativeLoop(t, nil, nil, 2)
	ctx := &cancelBetweenToolIntentChecks{Context: context.Background()}
	seq, err := loop.appendNativeToolIntent(ctx, 1, 1, session.ToolRequest{ID: "call", Name: "echo", Arguments: `{}`})
	if err != nil || seq != 0 {
		t.Fatalf("tool intent checkpoint: seq=%d err=%v", seq, err)
	}
	if events := s.DurableEvents(); len(events) != 1 || events[0].Type != "tool/call" {
		t.Fatalf("tool intent was not durable: %+v", events)
	}
}

func (m *scheduledNativeModel) Complete(context.Context, []Message, []ToolSpec) (session.AssistantMessage, error) {
	m.steps++
	if m.steps == 1 {
		return session.AssistantMessage{ToolCalls: m.calls}, nil
	}
	return session.AssistantMessage{Content: "done"}, nil
}

type scheduledNativeTool struct {
	name                string
	safe                bool
	started             chan<- string
	release             <-chan struct{}
	conclude            bool
	cancelAfterConclude func()
}

func (t *scheduledNativeTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Parameters: json.RawMessage(`{"type":"object"}`), ParallelSafe: t.safe}
}

func (*scheduledNativeTool) Validate(json.RawMessage) error { return nil }

func (t *scheduledNativeTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	t.started <- t.name
	select {
	case <-t.release:
		if t.conclude {
			ConcludeTurn(ctx)
			if t.cancelAfterConclude != nil {
				t.cancelAfterConclude()
			}
		}
		return t.name, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestNativeCancelledConcludingToolDoesNotReportSuccessfulTurn(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	calls := []session.ToolRequest{{ID: "finish", Name: "finish", Arguments: `{}`}}
	ctx, cancel := context.WithCancel(context.Background())
	tool := &scheduledNativeTool{name: "finish", started: started, release: release, conclude: true, cancelAfterConclude: cancel}
	loop, s := scheduledNativeLoop(t, calls, []Tool{tool}, 1)
	finished := make(chan error, 1)
	go func() { _, err := loop.RunTextTurn(ctx, "finish"); finished <- err }()
	awaitScheduledTool(t, started, "finish")
	close(release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled concluding turn returned %v", err)
	}
	if err := sessionformat.ValidateV4Relationships(s.DurableEvents()); err != nil {
		t.Fatal(err)
	}
}

func scheduledNativeLoop(t *testing.T, calls []session.ToolRequest, tools []Tool, limit int) (*NativeLoop, *sessionformat.LiveSession) {
	t.Helper()
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "scheduled", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(s, &scheduledNativeModel{calls: calls}, "provider", "model", "system", tools, 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.SetParallelToolLimit(limit); err != nil {
		t.Fatal(err)
	}
	return loop, s
}

func awaitScheduledTool(t *testing.T, started <-chan string, want string) {
	t.Helper()
	select {
	case got := <-started:
		if got != want {
			t.Fatalf("started %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("tool %q did not start", want)
	}
}

func awaitScheduledPair(t *testing.T, started <-chan string, first, second string) {
	t.Helper()
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case name := <-started:
			if name != first && name != second || seen[name] {
				t.Fatalf("unexpected parallel start %q; seen=%v", name, seen)
			}
			seen[name] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("parallel tools did not both start; seen=%v", seen)
		}
	}
}

func TestNativeParallelToolsRollSlotsAndCommitInModelOrder(t *testing.T) {
	started := make(chan string, 4)
	release := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{}), "c": make(chan struct{}), "exclusive": make(chan struct{})}
	calls := []session.ToolRequest{
		{ID: "a", Name: "a", Arguments: `{}`}, {ID: "b", Name: "b", Arguments: `{}`},
		{ID: "c", Name: "c", Arguments: `{}`}, {ID: "exclusive", Name: "exclusive", Arguments: `{}`},
	}
	var tools []Tool
	for _, call := range calls {
		tools = append(tools, &scheduledNativeTool{name: call.Name, safe: call.Name != "exclusive", started: started, release: release[call.Name]})
	}
	loop, s := scheduledNativeLoop(t, calls, tools, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := loop.RunTextTurn(ctx, "run tools"); finished <- err }()
	awaitScheduledPair(t, started, "a", "b")
	close(release["b"])
	awaitScheduledTool(t, started, "c")
	close(release["c"])
	select {
	case got := <-started:
		t.Fatalf("exclusive tool %q started before the parallel group drained", got)
	default:
	}
	close(release["a"])
	awaitScheduledTool(t, started, "exclusive")
	close(release["exclusive"])
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	var intents, results []string
	for _, event := range s.DurableEvents() {
		if event.Type != "tool/call" && event.Type != "tool/result" {
			continue
		}
		var data struct {
			CallID  string `json:"callId"`
			Message struct {
				ToolCallID string `json:"toolCallId"`
			} `json:"message"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		if event.Type == "tool/call" {
			intents = append(intents, data.CallID)
		} else {
			results = append(results, data.Message.ToolCallID)
		}
	}
	want := []string{"a", "b", "c", "exclusive"}
	for index := range want {
		if len(intents) != len(want) || len(results) != len(want) || intents[index] != want[index] || results[index] != want[index] {
			t.Fatalf("tool persistence order: intents=%v results=%v", intents, results)
		}
	}
	if err := sessionformat.ValidateV4Relationships(s.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("SERVERAGENTS_V4_PARALLEL_TOOL_EXPORT_PATH"); path != "" {
		journal := &nativeTestJournal{events: s.DurableEvents()}
		if err := os.WriteFile(path, nativeTestJournalJSONL(t, s.Header(), journal), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeParallelCancellationPairsUnstartedToolCalls(t *testing.T) {
	started := make(chan string, 3)
	release := make(chan struct{})
	calls := []session.ToolRequest{{ID: "a", Name: "a", Arguments: `{}`}, {ID: "b", Name: "b", Arguments: `{}`}, {ID: "c", Name: "c", Arguments: `{}`}}
	var tools []Tool
	for _, call := range calls {
		tools = append(tools, &scheduledNativeTool{name: call.Name, safe: true, started: started, release: release})
	}
	loop, s := scheduledNativeLoop(t, calls, tools, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	finished := make(chan error, 1)
	go func() { _, err := loop.RunTextTurn(ctx, "run tools"); finished <- err }()
	awaitScheduledPair(t, started, "a", "b")
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled turn returned %v", err)
	}
	select {
	case name := <-started:
		t.Fatalf("tool %q started after cancellation", name)
	default:
	}
	var intents, results int
	var skipped bool
	for _, event := range s.DurableEvents() {
		switch event.Type {
		case "tool/call":
			intents++
		case "tool/result":
			results++
			var data struct {
				Message struct {
					ToolCallID string `json:"toolCallId"`
				} `json:"message"`
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			skipped = skipped || data.Message.ToolCallID == "c" && data.Error.Code == "TOOL_ABORTED_BEFORE_DISPATCH"
		}
	}
	if intents != 3 || results != 3 || !skipped {
		t.Fatalf("cancel settlement: intents=%d results=%d skipped=%t", intents, results, skipped)
	}
	if err := sessionformat.ValidateV4Relationships(s.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("SERVERAGENTS_V4_CANCELLED_TOOL_EXPORT_PATH"); path != "" {
		journal := &nativeTestJournal{events: s.DurableEvents()}
		if err := os.WriteFile(path, nativeTestJournalJSONL(t, s.Header(), journal), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
