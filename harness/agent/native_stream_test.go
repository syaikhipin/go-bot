package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go-harness/harness/session"
	"go-harness/internal/modeldata"
	"go-harness/thirdparty/llm"
)

type nativeRetryStreamModel struct{ calls int }

func (*nativeRetryStreamModel) Complete(context.Context, []Message, []ToolSpec) (modeldata.AssistantMessage, error) {
	panic("native turn bypassed streaming model path")
}

func (m *nativeRetryStreamModel) StreamChunks(_ context.Context, _ []Message, _ []ToolSpec, sink ModelStreamSink) (modeldata.AssistantMessage, error) {
	m.calls++
	text := "done"
	if m.calls == 1 {
		text = "partial"
	}
	for _, chunk := range []ModelStreamChunk{
		{"type": "block-start", "index": 0, "blockType": "text"},
		{"type": "text-delta", "index": 0, "text": text},
	} {
		if err := sink(chunk); err != nil {
			return modeldata.AssistantMessage{}, err
		}
	}
	if m.calls == 1 {
		return modeldata.AssistantMessage{}, &llm.ModelError{Code: llm.ModelErrorUpstreamRateLimited, RetryAfter: time.Millisecond, Cause: errors.New("rate limited")}
	}
	for _, chunk := range []ModelStreamChunk{
		{"type": "block-end", "index": 0, "block": ModelStreamChunk{"type": "text", "text": text}},
		{"type": "usage", "usage": ModelStreamChunk{"inputTokens": 4, "outputTokens": 2}},
		{"type": "finish", "reason": ModelStreamChunk{"kind": "stop"}},
	} {
		if err := sink(chunk); err != nil {
			return modeldata.AssistantMessage{}, err
		}
	}
	return modeldata.AssistantMessage{Content: text, OutputBlocks: []modeldata.AssistantOutputBlock{{Type: "text", Text: text}},
		Usage: &modeldata.ModelUsage{InputTokens: 4, OutputTokens: 2, TotalTokens: 6}}, nil
}

func TestNativeLoopSettlesEachRealStreamAttemptBeforeRetryOrSurface(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-stream-retry", CreatedAt: 1}
	journal := &nativeTestJournal{}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeRetryStreamModel{}
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	frames, unsubscribe := loop.SubscribeAssistantStream()
	defer unsubscribe()
	loop.retryPolicy.wait = func(context.Context, time.Duration) error { return nil }
	if answer, err := loop.RunTextTurn(context.Background(), "answer"); err != nil || answer != "done" || model.calls != 2 {
		t.Fatalf("streaming retry = %q, %v; calls=%d", answer, err, model.calls)
	}
	var attempt, committed []json.RawMessage
	for _, event := range live.DurableEvents() {
		switch event.Type {
		case "assistant/attempt":
			attempt = append(attempt, event.Data)
		case "assistant/message":
			committed = append(committed, event.Data)
		}
	}
	if len(attempt) != 1 || len(committed) != 1 || !strings.Contains(string(attempt[0]), `"texts":["partial"]`) ||
		!strings.Contains(string(attempt[0]), `"kind":"error"`) || !strings.Contains(string(committed[0]), `"texts":["done"]`) ||
		strings.Contains(string(committed[0]), `"texts":["partial"]`) {
		t.Fatalf("attempt/surface settlement: attempt=%s committed=%s", attempt, committed)
	}
	var observed []NativeAssistantStreamFrame
	for {
		select {
		case frame := <-frames:
			observed = append(observed, frame)
		default:
			goto drained
		}
	}
drained:
	var starts, ends int
	var previousRevision uint64
	for _, frame := range observed {
		if frame.Revision <= previousRevision {
			t.Fatalf("non-monotone stream revisions: %+v", observed)
		}
		previousRevision = frame.Revision
		switch frame.Type {
		case "start":
			starts++
			if frame.Turn != 1 || frame.Step != 1 {
				t.Fatalf("incorrect attempt position: %+v", frame)
			}
		case "end":
			ends++
			want := "assistant/attempt"
			if ends == 2 {
				want = "assistant/message"
			}
			if frame.Outcome["kind"] != "committed" || frame.Outcome["eventType"] != want || frame.Index == nil {
				t.Fatalf("attempt end was published before durable settlement: %+v", frame)
			}
			seq, ok := frame.Outcome["seq"].(uint64)
			if !ok || int(seq) >= len(journal.events) || journal.events[seq].Type != want {
				t.Fatalf("attempt end references no committed %s: %+v", want, frame)
			}
		}
	}
	if starts != 2 || ends != 2 || observed[0].Type != "start" || observed[len(observed)-1].Type != "end" {
		t.Fatalf("incomplete assistant attempt frames: %+v", observed)
	}
	var event struct {
		Stream []struct {
			Type  string   `json:"type"`
			Texts []string `json:"texts"`
			Chunk struct {
				Type string `json:"type"`
			} `json:"chunk"`
		} `json:"stream"`
	}
	if err := json.Unmarshal(committed[0], &event); err != nil {
		t.Fatal(err)
	}
	var deltas int
	for _, record := range event.Stream {
		if record.Type == "text-chunks" && len(record.Texts) == 1 && record.Texts[0] == "done" {
			deltas++
		}
	}
	if deltas != 1 {
		t.Fatalf("committed stream did not contain the real delta: %s", committed[0])
	}
	if err := sessionformat.ValidateV4Rows(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("SERVERAGENTS_V4_STREAM_EXPORT_PATH"); path != "" {
		if err := os.WriteFile(path, nativeTestJournalJSONL(t, header, journal), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

type nativeCancelledStreamModel struct{ started chan struct{} }

func (*nativeCancelledStreamModel) Complete(context.Context, []Message, []ToolSpec) (modeldata.AssistantMessage, error) {
	panic("cancelled native turn bypassed streaming model path")
}

func (m *nativeCancelledStreamModel) StreamChunks(ctx context.Context, _ []Message, _ []ToolSpec, sink ModelStreamSink) (modeldata.AssistantMessage, error) {
	for _, chunk := range []ModelStreamChunk{
		{"type": "block-start", "index": 0, "blockType": "text"},
		{"type": "text-delta", "index": 0, "text": "safe prefix"},
		{"type": "block-start", "index": 1, "blockType": "tool-call"},
		{"type": "tool-call-delta", "index": 1, "id": "unfinished", "name": "shell", "argumentsDelta": "{"},
	} {
		if err := sink(chunk); err != nil {
			return modeldata.AssistantMessage{}, err
		}
	}
	close(m.started)
	<-ctx.Done()
	return modeldata.AssistantMessage{}, ctx.Err()
}

func TestNativeLoopCancelledStreamCommitsOnlySafeVisiblePrefix(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "cancelled-native-stream", CreatedAt: 1}
	journal := &nativeTestJournal{}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeCancelledStreamModel{started: make(chan struct{})}
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	frames, unsubscribe := loop.SubscribeAssistantStream()
	defer unsubscribe()
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := loop.RunTextTurn(ctx, "start")
		done <- err
	}()
	select {
	case <-model.started:
	case <-time.After(3 * time.Second):
		t.Fatal("streaming model did not start")
	}
	cancel(ErrNativeUserCancellation)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled stream = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled stream did not settle")
	}
	var assistant, aborted bool
	for _, event := range live.DurableEvents() {
		switch event.Type {
		case "assistant/message":
			assistant = strings.Contains(string(event.Data), `"interrupted":true`) && strings.Contains(string(event.Data), `"safe prefix"`) &&
				!strings.Contains(string(event.Data), `"arguments":"{"`)
		case "turn/end":
			aborted = strings.Contains(string(event.Data), `"kind":"aborted"`)
		case "tool/call":
			t.Fatal("cancelled partial tool was executed")
		}
	}
	if !assistant || !aborted {
		t.Fatalf("cancelled stream did not retain safe prefix and abort: %+v", live.DurableEvents())
	}
	var ended bool
	for {
		select {
		case frame := <-frames:
			ended = ended || frame.Type == "end" && frame.Outcome["kind"] == "committed" && frame.Outcome["eventType"] == "assistant/message"
		default:
			if !ended {
				t.Fatal("cancelled stream did not publish committed interrupted message")
			}
			if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
				t.Fatal(err)
			}
			if path := os.Getenv("SERVERAGENTS_V4_STREAM_CANCEL_EXPORT_PATH"); path != "" {
				if err := os.WriteFile(path, nativeTestJournalJSONL(t, header, journal), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			return
		}
	}
}

func TestNativeAssistantStreamBaselineReconstructsActiveAttempt(t *testing.T) {
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "baseline-native-stream", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeCancelledStreamModel{started: make(chan struct{})}
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, runErr := loop.RunTextTurn(ctx, "start")
		finished <- runErr
	}()
	select {
	case <-model.started:
	case <-time.After(3 * time.Second):
		t.Fatal("model never reached the active stream")
	}
	baseline, frames, unsubscribe := loop.SubscribeAssistantStreamSnapshot()
	defer unsubscribe()
	active := baseline.ActiveAttempt
	if active == nil || baseline.Revision != 5 || active.AttemptID == "" || active.Turn != 1 || active.Step != 1 ||
		active.NextIndex != 4 || active.StartedAfterSeq != live.DurableCursor() {
		t.Fatalf("active reconnect baseline = %+v", baseline)
	}
	encoded, err := json.Marshal(active.Stream)
	if err != nil || !strings.Contains(string(encoded), `"texts":["safe prefix"]`) ||
		!strings.Contains(string(encoded), `"args":["{"]`) {
		t.Fatalf("active compact prefix = %s, %v", encoded, err)
	}
	cancel(ErrNativeUserCancellation)
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled baseline turn = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("baseline turn did not stop")
	}
	var follow []NativeAssistantStreamFrame
	for {
		select {
		case frame := <-frames:
			follow = append(follow, frame)
		default:
			goto drained
		}
	}
drained:
	if len(follow) != 2 || follow[0].Type != "chunk" || follow[0].Index == nil || *follow[0].Index != active.NextIndex ||
		follow[1].Type != "end" || follow[1].Index == nil || *follow[1].Index != active.NextIndex+1 {
		t.Fatalf("baseline follow suffix is not dense: %+v", follow)
	}
	settled, settledFrames, cancelSettled := loop.SubscribeAssistantStreamSnapshot()
	defer cancelSettled()
	if settled.ActiveAttempt != nil || settled.Revision != follow[1].Revision || len(settledFrames) != 0 {
		t.Fatalf("settled reconnect baseline = %+v", settled)
	}
}

type nativePanicStreamModel struct{}

func (nativePanicStreamModel) Complete(context.Context, []Message, []ToolSpec) (modeldata.AssistantMessage, error) {
	panic("streaming path required")
}

func (nativePanicStreamModel) StreamChunks(_ context.Context, _ []Message, _ []ToolSpec, sink ModelStreamSink) (modeldata.AssistantMessage, error) {
	if err := sink(ModelStreamChunk{"type": "text-delta", "index": 0, "text": "uncommitted"}); err != nil {
		return modeldata.AssistantMessage{}, err
	}
	panic("provider adapter broke")
}

func TestNativeLoopPanickedStreamSettlesAttemptWithoutSurfaceMessage(t *testing.T) {
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "panic-native-stream", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(live, nativePanicStreamModel{}, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	frames, unsubscribe := loop.SubscribeAssistantStream()
	defer unsubscribe()
	if _, err := loop.RunTextTurn(context.Background(), "start"); err == nil || !strings.Contains(err.Error(), "provider adapter broke") {
		t.Fatalf("panicked stream = %v", err)
	}
	var attempt, surface bool
	for _, event := range live.DurableEvents() {
		switch event.Type {
		case "assistant/attempt":
			attempt = strings.Contains(string(event.Data), `"texts":["uncommitted"]`) && strings.Contains(string(event.Data), `"kind":"error"`)
		case "assistant/message":
			surface = true
		}
	}
	if !attempt || surface {
		t.Fatalf("panicked stream reached surface or lost attempt: %+v", live.DurableEvents())
	}
	var committedEnd bool
	for {
		select {
		case frame := <-frames:
			committedEnd = committedEnd || frame.Type == "end" && frame.Outcome["kind"] == "committed" && frame.Outcome["eventType"] == "assistant/attempt"
		default:
			if !committedEnd {
				t.Fatal("panicked stream lacked committed attempt end")
			}
			return
		}
	}
}
