package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"go-harness/harness/session"
	"go-harness/internal/modeldata"
)

type nativeTimeoutResultModel struct {
	calls int
	seen  [][]Message
}

func (m *nativeTimeoutResultModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (modeldata.AssistantMessage, error) {
	m.calls++
	m.seen = append(m.seen, append([]Message(nil), messages...))
	if m.calls == 1 {
		return modeldata.AssistantMessage{ToolCalls: []modeldata.ToolRequest{nativeTimeoutCall()}}, nil
	}
	return modeldata.AssistantMessage{Content: "done"}, nil
}

type nativeTimeoutProbe struct {
	timeout time.Duration
	run     func(context.Context) (string, error)
}

func (t nativeTimeoutProbe) Spec() ToolSpec {
	return ToolSpec{Name: "timeout-probe", Parameters: json.RawMessage(`{"type":"object"}`), Timeout: t.timeout}
}

func (nativeTimeoutProbe) Validate(json.RawMessage) error { return nil }

func (t nativeTimeoutProbe) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	return t.run(ctx)
}

func nativeTimeoutLoop(t *testing.T, tool Tool) *NativeLoop {
	t.Helper()
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "native-timeout-probe", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(live, &nativeAnswerModel{}, "provider", "model", "prompt", []Tool{tool}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	return loop
}

func nativeTimeoutCall() modeldata.ToolRequest {
	return modeldata.ToolRequest{ID: "call", Name: "timeout-probe", Arguments: `{}`}
}

func TestNativeToolInternalDeadlineIsNotHarnessTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Hour} {
		t.Run(timeout.String(), func(t *testing.T) {
			loop := nativeTimeoutLoop(t, nativeTimeoutProbe{timeout: timeout, run: func(context.Context) (string, error) {
				return "", context.DeadlineExceeded
			}})
			result := loop.executeNativeTool(context.Background(), 1, 1, nativeTimeoutCall())
			if !result.IsError || result.Code != "TOOL_ERROR" || result.Content != context.DeadlineExceeded.Error() {
				t.Fatalf("nested deadline was misclassified: %#v", result)
			}
		})
	}
}

func TestNativeToolHarnessTimeoutWaitsForToolSettlement(t *testing.T) {
	started := make(chan struct{})
	timedOut := make(chan struct{})
	release := make(chan struct{})
	loop := nativeTimeoutLoop(t, nativeTimeoutProbe{timeout: 20 * time.Millisecond, run: func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		close(timedOut)
		<-release
		return "late success", nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan modeldata.ToolResult, 1)
	go func() { finished <- loop.executeNativeTool(ctx, 1, 1, nativeTimeoutCall()) }()
	<-started
	select {
	case <-timedOut:
	case <-time.After(time.Second):
		t.Fatal("Harness deadline did not reach the tool")
	}
	select {
	case result := <-finished:
		t.Fatalf("Harness abandoned the tool before settlement: %#v", result)
	default:
	}
	// The Harness timer won. A later user cancellation must not erase its
	// already-established timeout cause while the tool is cleaning up.
	cancel()
	close(release)
	select {
	case result := <-finished:
		if !result.IsError || result.Code != "TOOL_TIMEOUT" || result.Content != "Error: tool call timed out after 20ms" {
			t.Fatalf("Harness deadline result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("tool did not settle after cleanup")
	}
}

func TestNativeToolCallerCancellationIsNotHarnessTimeout(t *testing.T) {
	started := make(chan struct{})
	loop := nativeTimeoutLoop(t, nativeTimeoutProbe{timeout: time.Hour, run: func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan modeldata.ToolResult, 1)
	go func() { finished <- loop.executeNativeTool(ctx, 1, 1, nativeTimeoutCall()) }()
	<-started
	cancel()
	select {
	case result := <-finished:
		if !result.IsError || result.Code == "TOOL_TIMEOUT" || !errors.Is(context.Cause(ctx), context.Canceled) {
			t.Fatalf("caller cancel was misclassified: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not settle the tool")
	}
}

func TestNativeToolParentDeadlineIsNotHarnessTimeout(t *testing.T) {
	loop := nativeTimeoutLoop(t, nativeTimeoutProbe{timeout: time.Hour, run: func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := loop.executeNativeTool(ctx, 1, 1, nativeTimeoutCall())
	if !result.IsError || result.Code == "TOOL_TIMEOUT" || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("parent deadline was misclassified: %#v", result)
	}
}

func TestNativeToolInternalDeadlinePersistsAsOrdinaryToolError(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-internal-deadline", CreatedAt: 1}
	journal := &nativeTestJournal{}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeTimeoutResultModel{}
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", []Tool{nativeTimeoutProbe{timeout: time.Hour, run: func(context.Context) (string, error) {
		return "", context.DeadlineExceeded
	}}}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := loop.RunTextTurn(context.Background(), "run the tool")
	if err != nil || answer != "done" || model.calls != 2 {
		t.Fatalf("turn = %q, calls=%d, err=%v", answer, model.calls, err)
	}
	var found bool
	for _, event := range journal.events {
		if event.Type != "tool/result" {
			continue
		}
		var payload struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil || payload.Error.Code != "TOOL_ERROR" {
			t.Fatalf("persisted tool error = %+v, %v", payload, err)
		}
		found = true
	}
	if !found {
		t.Fatal("missing persisted tool/result")
	}
	if len(model.seen[1]) == 0 || model.seen[1][len(model.seen[1])-1].Content != context.DeadlineExceeded.Error() {
		t.Fatalf("next request did not see the settled tool failure: %+v", model.seen[1])
	}
	raw := nativeTestJournalJSONL(t, header, journal)
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(raw)); err != nil {
		t.Fatalf("persisted timeout turn is not recoverable: %v", err)
	}
	if path := os.Getenv("SERVERAGENTS_V4_TOOL_TIMEOUT_EXPORT_PATH"); path != "" {
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeToolHarnessDeadlinePersistsOriginalTimeoutInfo(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-harness-deadline", CreatedAt: 1}
	journal := &nativeTestJournal{}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeTimeoutResultModel{}
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", []Tool{nativeTimeoutProbe{timeout: 20 * time.Millisecond, run: func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := loop.RunTextTurn(context.Background(), "run the tool")
	if err != nil || answer != "done" || model.calls != 2 {
		t.Fatalf("turn = %q, calls=%d, err=%v", answer, model.calls, err)
	}
	var found bool
	for _, event := range journal.events {
		if event.Type != "tool/result" {
			continue
		}
		var payload struct {
			Error struct {
				Name string `json:"name"`
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil || payload.Error.Name != "ToolTimeoutError" || payload.Error.Code != "TOOL_TIMEOUT" {
			t.Fatalf("persisted Harness timeout = %+v, %v", payload, err)
		}
		found = true
	}
	if !found {
		t.Fatal("missing persisted tool/result")
	}
	if len(model.seen[1]) == 0 || model.seen[1][len(model.seen[1])-1].Content != "Error: tool call timed out after 20ms" {
		t.Fatalf("next request did not see the Harness timeout: %+v", model.seen[1])
	}
	raw := nativeTestJournalJSONL(t, header, journal)
	if _, err := sessionformat.InspectV4JSONL(bytes.NewReader(raw)); err != nil {
		t.Fatalf("persisted Harness timeout turn is not recoverable: %v", err)
	}
	if path := os.Getenv("SERVERAGENTS_V4_HARNESS_TIMEOUT_EXPORT_PATH"); path != "" {
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
