package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-harness/harness/attachments"
	"go-harness/harness/session"
	"go-harness/internal/testsupport/session"
	"go-harness/thirdparty/llm"
)

type nativeTestJournal struct {
	events []sessionformat.Event
}

func (j *nativeTestJournal) Append(_ context.Context, events []sessionformat.Event) error {
	for _, event := range events {
		if event.Seq != uint64(len(j.events)) {
			return errors.New("noncontiguous v4 journal suffix")
		}
		j.events = append(j.events, event)
	}
	return nil
}
func (*nativeTestJournal) Close() error { return nil }

type nativeOneShotRollbackJournal struct {
	*nativeTestJournal
	failNext bool
}

func (j *nativeOneShotRollbackJournal) Append(ctx context.Context, events []sessionformat.Event) error {
	if j.failNext {
		j.failNext = false
		return sessionformat.ErrV4WriteRolledBack
	}
	return j.nativeTestJournal.Append(ctx, events)
}

func TestNativeLoopDoesNotCallModelAcrossRolledBackCheckpoint(t *testing.T) {
	ctx := context.Background()
	header := sessionformat.Header{Type: "session", Version: 4, ID: "rolled-back-checkpoint", CreatedAt: 1}
	journal := &nativeOneShotRollbackJournal{nativeTestJournal: &nativeTestJournal{}}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeAnswerModel{}
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.AcceptText(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	journal.failNext = true
	if ran, _, err := loop.RunNext(ctx); !ran || !errors.Is(err, sessionformat.ErrV4WriteRolledBack) {
		t.Fatalf("checkpoint failure did not end the first turn: ran=%v, err=%v", ran, err)
	}
	if model.calls != 0 {
		t.Fatal("model ran after the request checkpoint failed")
	}
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatalf("failed turn did not close durably: %v", err)
	}
	if _, err := loop.AcceptText(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if ran, answer, err := loop.RunNext(ctx); !ran || err != nil || answer != "answer-1" || model.calls != 1 {
		t.Fatalf("healthy writer did not continue after rollback: ran=%v answer=%q calls=%d err=%v", ran, answer, model.calls, err)
	}
}

func nativeTestJournalJSONL(t *testing.T, header sessionformat.Header, journal *nativeTestJournal) []byte {
	t.Helper()
	var raw bytes.Buffer
	first, err := sessionformat.EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	raw.Write(first)
	raw.WriteByte('\n')
	for index, event := range journal.events {
		line, err := sessionformat.EncodeEvent(event, uint64(index))
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(line)
		raw.WriteByte('\n')
	}
	return raw.Bytes()
}

type nativeTestModel struct {
	calls    int
	observed [][]Message
}

type nativeOrderedOutputModel struct {
	calls    int
	observed [][]Message
}

func (m *nativeOrderedOutputModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	m.observed = append(m.observed, append([]Message(nil), messages...))
	if m.calls == 1 {
		return session.AssistantMessage{
			Content: "before after", ToolCalls: []session.ToolRequest{{ID: "call-1", Name: "echo", Arguments: `{}`}},
			Usage: &session.ModelUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110, ReasoningTokens: 2},
			OutputBlocks: []session.AssistantOutputBlock{
				{Type: "reasoning", Text: "private analysis"}, {Type: "text", Text: "before"},
				{Type: "tool-call", ID: "call-1", Name: "echo", Arguments: `{}`}, {Type: "text", Text: " after"},
			},
		}, nil
	}
	return session.AssistantMessage{Content: "done", OutputBlocks: []session.AssistantOutputBlock{{Type: "text", Text: "done"}}}, nil
}

func TestNativeLoopWritesOrderedNonstreamingAssistantBlocks(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "ordered-output", CreatedAt: 1}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeOrderedOutputModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "system prompt", []Tool{&nativeTestTool{}}, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := loop.RunTextTurn(context.Background(), "use the tool"); err != nil || answer != "done" {
		t.Fatalf("ordered turn = %q, %v", answer, err)
	}
	if len(model.observed) != 2 || len(model.observed[1]) < 3 ||
		len(model.observed[1][2].OrderedAssistantBlocks) != 4 ||
		model.observed[1][2].OrderedAssistantBlocks[0].Type != "reasoning" ||
		model.observed[1][2].Content != "before\n after" {
		t.Fatalf("reasoning did not survive the next request projection: %+v", model.observed)
	}
	var found bool
	for _, event := range s.DurableEvents() {
		if event.Type != "assistant/message" || found {
			continue
		}
		found = true
		var record struct {
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			Stream []struct {
				Chunk struct {
					Type  string `json:"type"`
					Index int    `json:"index"`
				} `json:"chunk"`
			} `json:"stream"`
			Usage struct {
				ReasoningTokens int64 `json:"reasoningTokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(event.Data, &record); err != nil {
			t.Fatal(err)
		}
		if len(record.Message.Content) != 4 || record.Message.Content[0].Type != "reasoning" ||
			record.Message.Content[0].Text != "private analysis" || record.Message.Content[1].Text != "before" ||
			record.Message.Content[2].Type != "tool-call" || record.Message.Content[3].Text != " after" ||
			record.Usage.ReasoningTokens != 2 || len(record.Stream) != 10 || record.Stream[0].Chunk.Index != 0 || record.Stream[2].Chunk.Index != 1 ||
			record.Stream[4].Chunk.Index != 2 || record.Stream[6].Chunk.Index != 3 {
			t.Fatalf("v4 assistant blocks/settlement lost order: %+v", record)
		}
	}
	if !found {
		t.Fatal("ordered assistant message was not committed")
	}
	if err := sessionformat.ValidateV4Relationships(s.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("SERVERAGENTS_V4_ORDERED_OUTPUT_EXPORT_PATH"); path != "" {
		if err := os.WriteFile(path, nativeTestJournalJSONL(t, header, journal), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func (m *nativeTestModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	m.observed = append(m.observed, append([]Message(nil), messages...))
	if m.calls == 1 {
		return session.AssistantMessage{ToolCalls: []session.ToolRequest{{ID: "call-1", Name: "echo", Arguments: `{"text":"value"}`}}}, nil
	}
	return session.AssistantMessage{Content: "done", Usage: &session.ModelUsage{InputTokens: 12, OutputTokens: 3, TotalTokens: 15, CachedInputTokens: 2}}, nil
}

type nativeTestTool struct{ calls int }

func (*nativeTestTool) Spec() ToolSpec {
	return ToolSpec{Name: "echo", Description: "Echo an input", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (*nativeTestTool) Validate(raw json.RawMessage) error {
	var value map[string]any
	return json.Unmarshal(raw, &value)
}
func (t *nativeTestTool) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	t.calls++
	return string(raw), nil
}

func TestNativeLoopOneToolTurnProducesInspectibleV4Artifact(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native", CreatedAt: 1, CWD: "/workspace"}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model, tool := &nativeTestModel{}, &nativeTestTool{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "system prompt", []Tool{tool}, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := loop.RunTextTurn(context.Background(), "use the tool")
	if err != nil || answer != "done" || model.calls != 2 || tool.calls != 1 {
		t.Fatalf("native turn = %q, %v; model=%d tool=%d", answer, err, model.calls, tool.calls)
	}
	if len(model.observed[1]) != 4 || model.observed[1][3].Role != "tool" || model.observed[1][3].ToolName != "echo" {
		t.Fatalf("tool result absent from next model request: %+v", model.observed[1])
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw := nativeTestJournalJSONL(t, header, journal)
	if exportPath := os.Getenv("SERVERAGENTS_V4_TOOL_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, raw, 0o600); err != nil {
			t.Fatalf("export native v4 tool journal: %v", err)
		}
	}
	inspection, err := sessionformat.InspectV4JSONL(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("native turn does not restore as v4: %v", err)
	}
	var calls, results, headers, systemMessages int
	for _, event := range inspection.Events {
		switch event.Type {
		case "tool/call":
			calls++
		case "tool/result":
			results++
			if string(event.SourceEventSeqs) == "" {
				t.Fatal("tool/result lost tool/call source")
			}
		case "request/header":
			headers++
		case "system/message":
			systemMessages++
		}
	}
	if calls != 1 || results != 1 || headers != 1 || systemMessages != 1 || len(inspection.Surface.Messages) != 5 {
		t.Fatalf("unexpected v4 turn facts: calls=%d results=%d headers=%d systems=%d surface=%d", calls, results, headers, systemMessages, len(inspection.Surface.Messages))
	}
	last := inspection.Events[len(inspection.Events)-1]
	if last.Type != "turn/end" || !strings.Contains(string(last.Data), `"kind":"completed"`) {
		t.Fatalf("turn did not close: %+v", last)
	}
}

func TestNativeLoopRejectsSchemaInvalidToolCallBeforeToolCode(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-schema", CreatedAt: 1, CWD: "/workspace"}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	tool := &schemaProbeTool{schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`)}
	loop, err := NewNativeLoop(s, &nativeTestModel{}, "provider", "model", "system", []Tool{tool}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{`{}`, `{"answer":"one"}`, `{"answer":1,"extra":true}`} {
		result := loop.executeNativeTool(context.Background(), 1, 1, session.ToolRequest{ID: "call", Name: "schema-probe", Arguments: arguments})
		if !result.IsError || result.Code != "INVALID_ARGUMENTS" {
			t.Fatalf("arguments %s returned %+v", arguments, result)
		}
	}
	if tool.validations != 0 || tool.executions != 0 {
		t.Fatalf("invalid parameters reached tool: validate=%d execute=%d", tool.validations, tool.executions)
	}
	result := loop.executeNativeTool(context.Background(), 1, 1, session.ToolRequest{ID: "call", Name: "schema-probe", Arguments: `{"answer":1}`})
	if result.IsError || tool.validations != 1 || tool.executions != 1 {
		t.Fatalf("valid parameters: result=%+v validate=%d execute=%d", result, tool.validations, tool.executions)
	}
}

type nativeConcludingTool struct{}

func (nativeConcludingTool) Spec() ToolSpec {
	return ToolSpec{Name: "finish", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (nativeConcludingTool) Validate(json.RawMessage) error { return nil }
func (nativeConcludingTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	ConcludeTurn(ctx)
	return "finished", nil
}

type nativeTwoToolModel struct{}

func (nativeTwoToolModel) Complete(context.Context, []Message, []ToolSpec) (session.AssistantMessage, error) {
	return session.AssistantMessage{ToolCalls: []session.ToolRequest{
		{ID: "finish-call", Name: "finish", Arguments: `{}`},
		{ID: "late-call", Name: "schema-probe", Arguments: `{}`},
	}}, nil
}

func TestNativeLoopConcludingToolFinishesAfterLaterCallsInSameStep(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-terminal", CreatedAt: 1, CWD: "/workspace"}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	late := &schemaProbeTool{schema: json.RawMessage(`{"type":"object"}`)}
	loop, err := NewNativeLoop(s, nativeTwoToolModel{}, "provider", "model", "system", []Tool{nativeConcludingTool{}, late}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "finish now"); err != nil {
		t.Fatal(err)
	}
	if late.executions != 1 {
		t.Fatal("tool after concluding call did not finish the current step")
	}
	for _, event := range s.Events() {
		if event.Type == "tool/result" && strings.Contains(string(event.Data), "late-call") {
			if strings.Contains(string(event.Data), `"isError":true`) {
				t.Fatalf("later tool was incorrectly rejected after a concluding call: %s", event.Data)
			}
			return
		}
	}
	t.Fatal("later tool result was not durable")
}

func TestNativeLoopCancelPendingMatchesDeepSeekInboxClearOrder(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "cancel-pending", CreatedAt: 1, CWD: "/workspace"}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(s, &nativeTestModel{}, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.AcceptText(context.Background(), "next turn"); err != nil {
		t.Fatal(err)
	}
	steer, err := json.Marshal(nativeTextMessage("steer-1", "user", map[string]any{"kind": "user"}, "next step"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.SteerMessage(context.Background(), steer); err != nil {
		t.Fatal(err)
	}
	if err := loop.CancelPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	inbox, err := sessionformat.FoldInbox(journal.events)
	if err != nil || len(inbox.NextStep) != 0 || len(inbox.NextTurn) != 0 {
		t.Fatalf("cleared native inbox = %+v, %v", inbox, err)
	}
	if len(journal.events) != 4 {
		t.Fatalf("native inbox mutation count = %d", len(journal.events))
	}
	for index, target := range []string{"next-step", "next-turn"} {
		event := journal.events[index+2]
		var data struct {
			Target  string `json:"target"`
			Outcome string `json:"outcome"`
		}
		if event.Type != "agent/inbox/spliced" || json.Unmarshal(event.Data, &data) != nil || data.Target != target || data.Outcome != "canceled" {
			t.Fatalf("cancel splice %d = %+v", index, event)
		}
	}
}

func TestNativeLoopModelBudgetIncludesToolFollowup(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-budget", CreatedAt: 1, CWD: "/workspace"}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model, tool := &nativeTestModel{}, &nativeTestTool{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", []Tool{tool}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.SetModelCallLimit(1); err != nil {
		t.Fatal(err)
	}
	_, err = loop.RunTextTurn(context.Background(), "use one tool")
	if !errors.Is(err, ErrModelCallBudget) || model.calls != 1 || tool.calls != 1 {
		t.Fatalf("model budget: model=%d tool=%d error=%v", model.calls, tool.calls, err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "new turn"); err != nil || model.calls != 2 {
		t.Fatalf("model budget did not reset next turn: calls=%d error=%v", model.calls, err)
	}
}

type cancellingNativeModel struct{ cancel context.CancelFunc }

func (m cancellingNativeModel) Complete(ctx context.Context, _ []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	m.cancel()
	return session.AssistantMessage{}, ctx.Err()
}

func TestNativeLoopCancellationClosesTurnAsAborted(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-cancel", CreatedAt: 1}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop, err := NewNativeLoop(s, cancellingNativeModel{cancel: cancel}, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(ctx, "cancel me"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled turn returned %v", err)
	}
	events := s.DurableEvents()
	if len(events) < 2 || events[len(events)-1].Type != "turn/end" || !strings.Contains(string(events[len(events)-1].Data), `"kind":"aborted"`) {
		t.Fatalf("cancelled turn not durably closed: %+v", events)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_CANCEL_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, nativeTestJournalJSONL(t, header, journal), 0o600); err != nil {
			t.Fatalf("export native v4 canceled turn: %v", err)
		}
	}
}

type nativePanicTool struct{}

func (nativePanicTool) Spec() ToolSpec {
	return ToolSpec{Name: "echo", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (nativePanicTool) Validate(json.RawMessage) error { return nil }
func (nativePanicTool) Execute(context.Context, json.RawMessage) (string, error) {
	panic("tool crashed")
}

func TestNativeLoopToolPanicBecomesDurableUnknownOutcomeResult(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-panic", CreatedAt: 1}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeTestModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", []Tool{nativePanicTool{}}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := loop.RunTextTurn(context.Background(), "use tool"); err != nil || answer != "done" {
		t.Fatalf("panic recovery turn = %q, %v", answer, err)
	}
	var result *sessionformat.Event
	for _, event := range s.DurableEvents() {
		if event.Type == "tool/result" {
			copy := event
			result = &copy
		}
	}
	if result == nil || !strings.Contains(string(result.Data), session.ToolOutcomeUnknown) {
		t.Fatalf("tool panic was not recorded as unknown outcome: %+v", result)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

type nativeImageTool struct{}

func (nativeImageTool) Spec() ToolSpec {
	return ToolSpec{Name: "echo", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (nativeImageTool) Validate(json.RawMessage) error                           { return nil }
func (nativeImageTool) Execute(context.Context, json.RawMessage) (string, error) { return "", nil }
func (nativeImageTool) ExecuteContent(context.Context, json.RawMessage) (string, []session.ContentBlock, error) {
	return "image result", []session.ContentBlock{{
		Type: "image", ImageRefID: "sha256:abc", MIMEType: "image/png", Bytes: 100, Width: 10, Height: 10,
		OriginalDimensions: &session.ImageDimensions{Width: 20, Height: 20},
	}}, nil
}

func TestNativeLoopPersistsToolImageAsNativeAttachmentReference(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-image", CreatedAt: 1}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeTestModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", []Tool{nativeImageTool{}}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "read image"); err != nil {
		t.Fatal(err)
	}
	lastRequest := model.observed[1]
	if len(lastRequest) != 4 || len(lastRequest[3].ContentBlocks) != 2 || lastRequest[3].ContentBlocks[0].Text != "image result" || lastRequest[3].ContentBlocks[1].ImageRefID != "sha256:abc" || lastRequest[3].ContentBlocks[1].OriginalDimensions == nil || lastRequest[3].ContentBlocks[1].OriginalDimensions.Width != 20 {
		t.Fatalf("image attachment disappeared from model surface: %+v", lastRequest)
	}
	var result *sessionformat.Event
	for _, event := range s.DurableEvents() {
		if event.Type == "tool/result" {
			copy := event
			result = &copy
		}
	}
	if result == nil || !strings.Contains(string(result.Data), `"attachmentId":"sha256:abc"`) || !strings.Contains(string(result.Data), `"originalDimensions":{"width":20,"height":20}`) || strings.Contains(string(result.Data), `"mimeType"`) {
		t.Fatalf("tool image was not written as native v4 reference: %+v", result)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_IMAGE_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, nativeTestJournalJSONL(t, header, journal), 0o600); err != nil {
			t.Fatalf("export native v4 image journal: %v", err)
		}
	}
}

func TestNativeModelProjectionPreservesMixedContentOrder(t *testing.T) {
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "mixed-content", CreatedAt: 1, CWD: "/workspace"}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data := json.RawMessage(`{"id":"user-1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"before"},{"type":"image","attachment":{"attachmentId":"sha256:one","mediaType":"image/png","bytes":10,"width":1,"height":1}},{"type":"text","text":"between"},{"type":"image","attachment":{"attachmentId":"sha256:two","mediaType":"image/png","bytes":10,"width":1,"height":1}},{"type":"text","text":"after"}]}`)
	if _, err := s.Append(sessionformat.EventInput{Type: "user/message", Data: data, SurfaceOp: json.RawMessage(`"append"`)}); err != nil {
		t.Fatal(err)
	}
	messages, err := nativeModelMessages(s, nil)
	if err != nil || len(messages) != 1 {
		t.Fatalf("native mixed message = %+v, %v", messages, err)
	}
	message := messages[0]
	if message.Content != "" || len(message.ContentBlocks) != 5 || message.ContentBlocks[0].Text != "before" ||
		message.ContentBlocks[1].ImageRefID != "sha256:one" || message.ContentBlocks[2].Text != "between" ||
		message.ContentBlocks[3].ImageRefID != "sha256:two" || message.ContentBlocks[4].Text != "after" {
		t.Fatalf("v4 block order changed before LLMAdapter: %+v", message)
	}
}

type nativeImageBudgetModel struct {
	calls     int
	active    []int
	alwaysAsk bool
}

func (m *nativeImageBudgetModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	active := 0
	for _, flag := range modelImageOffloadFlags(messages) {
		if !flag {
			active++
		}
	}
	m.active = append(m.active, active)
	if active > 1 || m.alwaysAsk {
		return session.AssistantMessage{}, &ImageOffloadRequiredError{Count: 1}
	}
	return session.AssistantMessage{Content: "image accepted"}, nil
}

func TestNativeLoopOffloadsOldestImageAndRebuildsRequestWithoutProviderRetry(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-image-offload", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeImageBudgetModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	message := json.RawMessage(`{"id":"image-user","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"Compare these"},{"type":"image","attachment":{"attachmentId":"image-a","mediaType":"image/png","bytes":10}},{"type":"image","attachment":{"attachmentId":"image-a","mediaType":"image/png","bytes":10}}]}`)
	if _, err := loop.AcceptMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if ran, answer, err := loop.RunNext(context.Background()); err != nil || !ran || answer != "image accepted" {
		t.Fatalf("image turn = %v, %q, %v", ran, answer, err)
	}
	if model.calls != 2 || len(model.active) != 2 || model.active[0] != 2 || model.active[1] != 1 {
		t.Fatalf("model image visibility = %v across %d calls", model.active, model.calls)
	}
	var userSeq uint64
	var users, steps, offloads, retries int
	for _, event := range s.Events() {
		switch event.Type {
		case "user/message":
			users++
			userSeq = event.Seq
		case "step/start":
			steps++
		case "image/offload":
			offloads++
			var decision imageOffloadEvent
			if err := json.Unmarshal(event.Data, &decision); err != nil || len(decision.Targets) != 1 || decision.Targets[0].Seq != userSeq || len(decision.Targets[0].ImageIndexes) != 1 || decision.Targets[0].ImageIndexes[0] != 0 {
				t.Fatalf("wrong native offload target: %s, %v", event.Data, err)
			}
		case "llm/retry":
			retries++
		}
	}
	if users != 1 || steps != 1 || offloads != 1 || retries != 0 {
		t.Fatalf("unexpected replayed work: users=%d steps=%d offloads=%d retries=%d", users, steps, offloads, retries)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionformat.FoldSurface(s.Events()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeV4OffloadResolvesToolWorldPathWithoutLoggingIt(t *testing.T) {
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "native-offload-path", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ref := session.ImageAttachment{ID: "sha256:" + strings.Repeat("a", 64), MIMEType: "image/png", Bytes: 6, Width: 2, Height: 3, Name: "photo.png"}
	message := json.RawMessage(`{"id":"image-user","role":"user","source":{"kind":"user"},"content":[{"type":"image","attachment":{"attachmentId":"` + ref.ID + `","mediaType":"image/png","bytes":6,"width":2,"height":3,"name":"photo.png"}},{"type":"image","attachment":{"attachmentId":"` + ref.ID + `","mediaType":"image/png","bytes":6,"width":2,"height":3,"name":"photo.png"}}]}`)
	if _, err := s.Append(sessionformat.EventInput{Type: "user/message", Data: message, SurfaceOp: json.RawMessage(`"append"`)}); err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(s, &nativeTestModel{}, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if selected, err := loop.offloadOldestNativeImages(1); err != nil || !selected {
		t.Fatalf("native offload = %v, %v", selected, err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	messages, err := nativeModelMessages(s, nil)
	if err != nil || len(messages) != 1 || len(messages[0].ContentBlocks) != 2 || messages[0].ContentBlocks[0].ReadPath != "" {
		t.Fatalf("native image projection unexpectedly stored source path: %+v, %v", messages, err)
	}
	reader := &recordingImageReader{data: []byte("pixels"), path: "/root/harness/.dsh/attachments/v1/objects/aa/" + strings.Repeat("a", 64)}
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed"}}
	adapter := LLMAdapter{Transport: transport, Images: reader, ImageInputEnabled: true}
	if _, err := adapter.Complete(context.Background(), messages, nil); err != nil {
		t.Fatal(err)
	}
	blocks := transport.request.Input[0].Content
	if len(blocks) != 2 || !strings.Contains(blocks[0].Text, reader.path) || blocks[0].Source != nil || blocks[1].Source == nil || reader.loads != 1 {
		t.Fatalf("v4 offload request did not retain re-read access and second occurrence: %+v", transport.request)
	}
	for _, event := range s.Events() {
		if event.Type == "image/offload" && strings.Contains(string(event.Data), reader.path) {
			t.Fatalf("tool-world path leaked into native v4 log: %s", event.Data)
		}
	}
}

func TestNativeV4OffloadColdRestoreReadsNormalizedImageAfterSourceChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces", "image-session")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	bitmap := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	bitmap.Set(0, 0, color.NRGBA{R: 255, A: 255})
	bitmap.Set(1, 0, color.NRGBA{G: 255, A: 255})
	if err := png.Encode(&encoded, bitmap); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(workspace, "original.png")
	if err := os.WriteFile(source, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	objectRoot := filepath.Join(root, ".harness")
	images := attachments.Store{API: attachments.LocalObjectAPI{Root: objectRoot}}
	ref, err := images.SaveImage(ctx, encoded.Bytes(), "image/png", "original.png", source)
	if err != nil {
		t.Fatal(err)
	}
	claims := &session.ProcessClaims{}
	files := sessionformat.FileStore{Root: filepath.Join(objectRoot, "sessions"), ProjectCWD: workspace, Claim: claims.Acquire}
	header := sessionformat.Header{Type: "session", Version: 4, ID: "image-session", CreatedAt: 1, CWD: workspace}
	journal, err := files.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeImageBudgetModel{}
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	attachment := map[string]any{"attachmentId": ref.ID, "mediaType": ref.MIMEType, "bytes": ref.Bytes, "width": ref.Width, "height": ref.Height, "name": ref.Name}
	block := map[string]any{"type": "image", "attachment": attachment}
	message, err := json.Marshal(map[string]any{"id": "image-user", "role": "user", "source": map[string]any{"kind": "user"}, "content": []any{block, block}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.AcceptMessage(ctx, message); err != nil {
		t.Fatal(err)
	}
	if ran, answer, err := loop.RunNext(ctx); err != nil || !ran || answer != "image accepted" {
		t.Fatalf("first image turn = %v, %q, %v", ran, answer, err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("the original image changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, artifact, err := files.Open(ctx, header.ID)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := sessionformat.RestoreLiveSession(artifact, reopened)
	if err != nil {
		_ = reopened.Close()
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	modelMessages, err := nativeModelMessages(restored, nil)
	if err != nil {
		t.Fatal(err)
	}
	path, err := attachments.LocalImageObjectPath(objectRoot, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedBytes, err := images.LoadImage(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk, err := os.ReadFile(path); err != nil || !bytes.Equal(onDisk, storedBytes) {
		t.Fatalf("normalized saved copy = %d bytes, %v", len(onDisk), err)
	}
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed"}}
	adapter := LLMAdapter{Transport: transport, Images: images, ImageAccessPath: images.ImageHostPath, ImageInputEnabled: true}
	if _, err := adapter.Complete(ctx, modelMessages, nil); err != nil {
		t.Fatal(err)
	}
	var offloaded, retained bool
	for _, item := range transport.request.Input {
		if item.Role != "user" {
			continue
		}
		for _, content := range item.Content {
			if content.Type == "text" && strings.Contains(content.Text, path) && !strings.Contains(content.Text, source) {
				offloaded = true
			}
			if content.Type == "image" && content.Source != nil {
				retained = true
			}
		}
	}
	if !offloaded || !retained {
		t.Fatalf("restored image request lost exact offload occurrence: offloaded=%v retained=%v request=%+v", offloaded, retained, transport.request)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_OFFLOAD_RESTORE_EXPORT_PATH"); exportPath != "" {
		filename, err := files.Path(header.ID)
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(exportPath, artifact, 0o600); err != nil {
			t.Fatalf("export cold-restored image offload journal: %v", err)
		}
	}
}

func TestNativeLoopStopsImageOffloadWhenNoCandidateRemains(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-image-exhausted", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeImageBudgetModel{alwaysAsk: true}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	message := json.RawMessage(`{"id":"image-user","role":"user","source":{"kind":"user"},"content":[{"type":"image","attachment":{"attachmentId":"image-a","mediaType":"image/png","bytes":10}}]}`)
	if _, err := loop.AcceptMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if ran, _, err := loop.RunNext(context.Background()); !ran || err == nil || model.calls != 2 {
		t.Fatalf("exhausted image turn = ran %v, err %v, calls %d", ran, err, model.calls)
	}
	var offloads int
	for _, event := range s.Events() {
		if event.Type == "image/offload" {
			offloads++
		}
	}
	if offloads != 1 {
		t.Fatalf("exhausted image offload count = %d", offloads)
	}
}

type nativeAnswerModel struct{ calls int }

func (m *nativeAnswerModel) Complete(_ context.Context, _ []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	return session.AssistantMessage{Content: fmt.Sprintf("answer-%d", m.calls)}, nil
}

func TestNativeLoopPreStepRejectClosesBlockedTurnWithoutModelStep(t *testing.T) {
	ctx := context.Background()
	header := sessionformat.Header{Type: "session", Version: 4, ID: "pre-step-reject", CreatedAt: 1}
	journal := &nativeTestJournal{}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeAnswerModel{}
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	loop.SetPreStepHook(func(context.Context) (NativePreStepDecision, error) {
		return NativePreStepReject, nil
	})
	if answer, err := loop.RunTextTurn(ctx, "reject this turn"); err != nil || answer != "" {
		t.Fatalf("rejected turn = %q, %v", answer, err)
	}
	if model.calls != 0 || len(live.Inbox().NextTurn) != 0 {
		t.Fatalf("rejected turn called model or retained input: calls=%d inbox=%+v", model.calls, live.Inbox())
	}
	var starts, ends, blocked int
	for _, event := range live.DurableEvents() {
		switch event.Type {
		case "turn/start":
			starts++
		case "turn/end":
			ends++
			if string(event.Data) == `{"reason":{"kind":"blocked"},"turn":1}` {
				blocked++
			}
		case "step/start", "step/end", "user/message", "assistant/message":
			t.Fatalf("rejected turn recorded %s", event.Type)
		}
	}
	if starts != 1 || ends != 1 || blocked != 1 {
		t.Fatalf("rejected turn boundaries start=%d end=%d blocked=%d", starts, ends, blocked)
	}
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	loop.SetPreStepHook(nil)
	if answer, err := loop.RunTextTurn(ctx, "next turn"); err != nil || answer != "answer-1" {
		t.Fatalf("next turn = %q, %v", answer, err)
	}
}

func TestNativeLoopSeparatesAdmissionFromOrderedTurnExecution(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-queue", CreatedAt: 1}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeAnswerModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := loop.AcceptText(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := loop.AcceptText(context.Background(), "second")
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID || len(s.Inbox().NextTurn) != 2 || model.calls != 0 {
		t.Fatalf("admission executed work or lost queue: inbox=%+v model=%d", s.Inbox(), model.calls)
	}
	if ran, answer, err := loop.RunNext(context.Background()); err != nil || !ran || answer != "answer-1" {
		t.Fatalf("first dequeue = %v, %q, %v", ran, answer, err)
	}
	if len(s.Inbox().NextTurn) != 1 {
		t.Fatalf("first turn did not claim exactly one input: %+v", s.Inbox())
	}
	if ran, answer, err := loop.RunNext(context.Background()); err != nil || !ran || answer != "answer-2" {
		t.Fatalf("second dequeue = %v, %q, %v", ran, answer, err)
	}
	if ran, _, err := loop.RunNext(context.Background()); err != nil || ran {
		t.Fatalf("empty queue should not run: %v, %v", ran, err)
	}
	var userIDs []string
	for _, event := range s.DurableEvents() {
		if event.Type != "user/message" {
			continue
		}
		var message struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(event.Data, &message); err != nil {
			t.Fatal(err)
		}
		userIDs = append(userIDs, message.ID)
	}
	if len(userIDs) != 2 || userIDs[0] != firstID || userIDs[1] != secondID {
		t.Fatalf("dequeued input order/identity changed: %v", userIDs)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeLoopResumesPendingInboxFromV4Events(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-resume-inbox", CreatedAt: 1}
	firstJournal := &nativeTestJournal{}
	first, err := sessionformat.NewLiveSession(header, firstJournal)
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(first, &nativeAnswerModel{}, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := loop.AcceptText(context.Background(), "pending before restart")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	stored := first.DurableEvents()
	secondJournal := &nativeTestJournal{events: append([]sessionformat.Event(nil), stored...)}
	restored, err := sessionformat.RestoreLiveSession(sessionformat.PhysicalArtifact{Header: header, Events: stored}, secondJournal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeAnswerModel{}
	resumed, err := NewNativeLoop(restored, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ran, answer, err := resumed.RunNext(context.Background()); err != nil || !ran || answer != "answer-1" {
		t.Fatalf("restored pending input = %v, %q, %v", ran, answer, err)
	}
	if len(restored.Inbox().NextTurn) != 0 {
		t.Fatalf("restored inbox was not claimed: %+v", restored.Inbox())
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, event := range secondJournal.events {
		if event.Type == "user/message" && strings.Contains(string(event.Data), messageID) {
			seen = true
		}
	}
	if !seen {
		t.Fatal("resumed turn lost pending message identity")
	}
}

func TestNativeLoopRepairsOpenToolTurnBeforeClaimingPendingInput(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-repair", CreatedAt: 1}
	firstJournal := &nativeTestJournal{}
	first, err := sessionformat.NewLiveSession(header, firstJournal)
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(eventType, data, op string) {
		t.Helper()
		if _, err := first.Append(sessionformat.EventInput{Type: eventType, Data: json.RawMessage(data), SurfaceOp: json.RawMessage(op)}); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent("turn/start", `{"turn":1}`, "")
	appendEvent("step/start", `{"turn":1,"step":1}`, "")
	appendEvent("system/message", `{"turn":1,"step":1,"message":{"id":"system","role":"system","source":{"kind":"system-prompt"},"content":[{"type":"text","text":"prompt"}]}}`, `"append"`)
	appendEvent("assistant/message", `{"turn":1,"step":1,"message":{"id":"a","role":"assistant","source":{"kind":"model","provider":"provider","model":"model"},"content":[{"type":"tool-call","id":"call-1","name":"echo","arguments":"{}"}]},"stream":[]}`, `"append"`)
	appendEvent("tool/call", `{"turn":1,"step":1,"callId":"call-1","name":"echo","arguments":"{}"}`, "")
	appendEvent("agent/inbox/spliced", `{"target":"next-turn","start":0,"inserted":[{"id":"queued","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"continue"}]}]}`, "")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	stored := first.DurableEvents()
	secondJournal := &nativeTestJournal{events: append([]sessionformat.Event(nil), stored...)}
	restored, err := sessionformat.RestoreLiveSession(sessionformat.PhysicalArtifact{Header: header, Events: stored}, secondJournal)
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(restored, &nativeAnswerModel{}, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ran, answer, err := loop.RunNext(context.Background()); err != nil || !ran || answer != "answer-1" {
		t.Fatalf("repaired next turn = %v, %q, %v", ran, answer, err)
	}
	var interrupted, completed, recoveredResult bool
	for _, event := range restored.DurableEvents() {
		if event.Type == "tool/result" && strings.Contains(string(event.Data), session.ToolOutcomeUnknown) {
			recoveredResult = true
		}
		if event.Type == "turn/end" && strings.Contains(string(event.Data), `"kind":"interrupted"`) {
			interrupted = true
		}
		if event.Type == "turn/end" && strings.Contains(string(event.Data), `"kind":"completed"`) {
			completed = true
		}
	}
	if !interrupted || !completed || !recoveredResult || len(restored.Inbox().NextTurn) != 0 {
		t.Fatalf("repair/claim sequence missing: interrupted=%v completed=%v result=%v inbox=%+v", interrupted, completed, recoveredResult, restored.Inbox())
	}
	if err := sessionformat.ValidateV4Relationships(restored.Events()); err != nil {
		t.Fatalf("repaired and resumed log is invalid: %v", err)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
}

type nativeRetryModel struct {
	calls    int
	journal  *nativeTestJournal
	observed [][]Message
}

func (m *nativeRetryModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	m.observed = append(m.observed, append([]Message(nil), messages...))
	switch m.calls {
	case 1:
		return session.AssistantMessage{}, &llm.ModelError{Code: llm.ModelErrorUpstreamRateLimited, HTTPStatus: 429,
			RetryAfter: time.Millisecond, Cause: errors.New("rate limited")}
	case 2:
		if m.journal.events[len(m.journal.events)-1].Type != "llm/retry-started" {
			return session.AssistantMessage{}, errors.New("retry-started was not durable before second request")
		}
		return session.AssistantMessage{ToolCalls: []session.ToolRequest{{ID: "call-1", Name: "echo", Arguments: `{}`}}}, nil
	default:
		return session.AssistantMessage{Content: "finished"}, nil
	}
}

func TestNativeLoopRetryKeepsOneStepAndDoesNotRepeatTool(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-retry", CreatedAt: 1, CWD: "/workspace"}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model, tool := &nativeRetryModel{journal: journal}, &nativeTestTool{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", []Tool{tool}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	loop.retryPolicy.wait = func(_ context.Context, delay time.Duration) error {
		if delay != time.Millisecond || journal.events[len(journal.events)-1].Type != "llm/retry" {
			t.Fatalf("retry was not durable before wait: delay=%s events=%+v", delay, journal.events)
		}
		return nil
	}
	answer, err := loop.RunTextTurn(context.Background(), "use tool")
	if err != nil || answer != "finished" || model.calls != 3 || tool.calls != 1 {
		t.Fatalf("retry turn = %q, %v; model calls=%d tool calls=%d", answer, err, model.calls, tool.calls)
	}
	first, _ := json.Marshal(model.observed[0])
	second, _ := json.Marshal(model.observed[1])
	if !bytes.Equal(first, second) {
		t.Fatalf("retry changed model request: first=%s second=%s", first, second)
	}
	counts := map[string]int{}
	for _, event := range s.DurableEvents() {
		counts[event.Type]++
		if event.Type == "llm/retry" && (!strings.Contains(string(event.Data), `"code":"RATE_LIMIT"`) || !strings.Contains(string(event.Data), `"maxRetries":5`)) {
			t.Fatalf("retry payload is not native DeepSeek: %s", event.Data)
		}
	}
	for kind, want := range map[string]int{"user/message": 1, "step/start": 2, "assistant/attempt": 1,
		"llm/retry": 1, "llm/retry-started": 1, "tool/call": 1, "tool/result": 1, "turn/end": 1} {
		if counts[kind] != want {
			t.Fatalf("%s count=%d want=%d", kind, counts[kind], want)
		}
	}
	if err := sessionformat.ValidateV4Rows(s.Events()); err != nil {
		t.Fatal(err)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_RETRY_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, nativeTestJournalJSONL(t, header, journal), 0o600); err != nil {
			t.Fatalf("export native v4 retried turn: %v", err)
		}
	}
}

type nativeTerminalModel struct{ calls int }

func (m *nativeTerminalModel) Complete(context.Context, []Message, []ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	return session.AssistantMessage{}, &llm.ModelError{Code: llm.ModelErrorInvalidRequest,
		HTTPStatus: 400, Cause: errors.New("bad request")}
}

func TestNativeLoopDoesNotRetryTerminalModelFailure(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-terminal", CreatedAt: 1}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeTerminalModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "fail"); err == nil {
		t.Fatal("terminal failure was hidden")
	}
	if model.calls != 1 {
		t.Fatalf("terminal failure made %d model calls", model.calls)
	}
	var retries, attempts int
	for _, event := range s.DurableEvents() {
		switch event.Type {
		case "llm/retry":
			retries++
		case "assistant/attempt":
			attempts++
		case "turn/end":
			if !strings.Contains(string(event.Data), `"code":"AI_INVALID_REQUEST"`) {
				t.Fatalf("terminal code lost from turn/end: %s", event.Data)
			}
		}
	}
	if retries != 0 || attempts != 1 {
		t.Fatalf("terminal failure logged retries=%d attempts=%d", retries, attempts)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_FAILURE_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, nativeTestJournalJSONL(t, header, journal), 0o600); err != nil {
			t.Fatalf("export native v4 terminal failure: %v", err)
		}
	}
}

type nativeEmptyThenAnswerModel struct{ calls int }

func (m *nativeEmptyThenAnswerModel) Complete(context.Context, []Message, []ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	if m.calls == 1 {
		return session.AssistantMessage{}, nil
	}
	return session.AssistantMessage{Content: "not empty"}, nil
}

func TestNativeLoopRetriesEmptyCompletionWithoutPublishingIt(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-empty", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeEmptyThenAnswerModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	loop.retryPolicy.wait = func(context.Context, time.Duration) error { return nil }
	if answer, err := loop.RunTextTurn(context.Background(), "answer"); err != nil || answer != "not empty" || model.calls != 2 {
		t.Fatalf("empty completion recovery = %q, %v; calls=%d", answer, err, model.calls)
	}
	var attempts, assistants int
	for _, event := range s.DurableEvents() {
		switch event.Type {
		case "assistant/attempt":
			attempts++
		case "assistant/message":
			assistants++
		case "llm/retry":
			if !strings.Contains(string(event.Data), `"code":"EMPTY_RESPONSE"`) {
				t.Fatalf("wrong failure code: %s", event.Data)
			}
		}
	}
	if attempts != 1 || assistants != 1 {
		t.Fatalf("empty attempt reached model surface: attempts=%d assistants=%d", attempts, assistants)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

type nativeReasoningOnlyModel struct{ calls int }

func (m *nativeReasoningOnlyModel) Complete(context.Context, []Message, []ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	return session.AssistantMessage{OutputBlocks: []session.AssistantOutputBlock{{Type: "reasoning", Text: "thinking only"}}}, nil
}

func TestNativeLoopReasoningOnlyCompletionDoesNotRetry(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-reasoning-only", CreatedAt: 1}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeReasoningOnlyModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	loop.retryPolicy.wait = func(context.Context, time.Duration) error {
		t.Fatal("reasoning-only completion entered the retry path")
		return nil
	}
	answer, err := loop.RunTextTurn(context.Background(), "think")
	if err != nil || answer != "" || model.calls != 1 {
		t.Fatalf("reasoning-only turn = %q, %v; model calls=%d", answer, err, model.calls)
	}
	var assistants, attempts, retries int
	for _, event := range s.DurableEvents() {
		switch event.Type {
		case "assistant/message":
			assistants++
			if !strings.Contains(string(event.Data), `"type":"reasoning"`) || !strings.Contains(string(event.Data), "thinking only") {
				t.Fatalf("reasoning block missing from assistant message: %s", event.Data)
			}
		case "assistant/attempt":
			attempts++
		case "llm/retry":
			retries++
		case "turn/end":
			if !strings.Contains(string(event.Data), `"kind":"completed"`) {
				t.Fatalf("reasoning-only turn did not complete: %s", event.Data)
			}
		}
	}
	if assistants != 1 || attempts != 0 || retries != 0 {
		t.Fatalf("reasoning-only settlement: assistants=%d attempts=%d retries=%d", assistants, attempts, retries)
	}
	if err := sessionformat.ValidateV4Rows(s.Events()); err != nil {
		t.Fatal(err)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_REASONING_ONLY_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, nativeTestJournalJSONL(t, header, journal), 0o600); err != nil {
			t.Fatalf("export reasoning-only v4 turn: %v", err)
		}
	}
}

func TestNativeLoopCancellationDuringRetryWaitDoesNotStartAnotherRequest(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-retry-cancel", CreatedAt: 1}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeRetryModel{journal: journal}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.retryPolicy.wait = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	if _, err := loop.RunTextTurn(ctx, "cancel retry"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retry returned %v", err)
	}
	if model.calls != 1 {
		t.Fatalf("cancellation caused %d model attempts", model.calls)
	}
	var scheduled, started, aborted bool
	for _, event := range s.DurableEvents() {
		switch event.Type {
		case "llm/retry":
			scheduled = true
		case "llm/retry-started":
			started = true
		case "turn/end":
			aborted = strings.Contains(string(event.Data), `"kind":"aborted"`)
		}
	}
	if !scheduled || started || !aborted {
		t.Fatalf("cancelled retry log: scheduled=%v started=%v aborted=%v", scheduled, started, aborted)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

type nativeHoldingModel struct {
	started  chan struct{}
	release  chan struct{}
	calls    int
	observed [][]Message
}

func (m *nativeHoldingModel) Complete(ctx context.Context, messages []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	m.observed = append(m.observed, append([]Message(nil), messages...))
	if m.calls == 1 {
		close(m.started)
		select {
		case <-m.release:
		case <-ctx.Done():
			return session.AssistantMessage{}, ctx.Err()
		}
	}
	return session.AssistantMessage{Content: fmt.Sprintf("answer-%d", m.calls)}, nil
}

func TestNativeLoopSteerJoinsCurrentTurnAtNextStep(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-steer", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeHoldingModel{started: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-model.release:
		default:
			close(model.release)
		}
	}()
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.AcceptText(context.Background(), "initial"); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		ran    bool
		answer string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		ran, answer, runErr := loop.RunNext(context.Background())
		done <- outcome{ran, answer, runErr}
	}()
	select {
	case <-model.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first step did not enter model")
	}
	steer, err := json.Marshal(nativeTextMessage("steer-1", "user", map[string]any{"kind": "user"}, "steer now"))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := loop.SteerMessage(context.Background(), steer); err != nil || id != "steer-1" {
		t.Fatalf("steer admission = %q, %v", id, err)
	}
	if len(s.Inbox().NextStep) != 1 {
		t.Fatalf("steer did not persist in next-step queue: %+v", s.Inbox())
	}
	close(model.release)
	select {
	case result := <-done:
		if !result.ran || result.answer != "answer-2" || result.err != nil {
			t.Fatalf("steered turn = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("steered next step did not finish")
	}
	if model.calls != 2 || len(model.observed[1]) != 4 || model.observed[1][3].Content != "steer now" {
		t.Fatalf("steer missing from second model request: calls=%d messages=%+v", model.calls, model.observed)
	}
	counts := map[string]int{}
	for _, event := range s.DurableEvents() {
		counts[event.Type]++
	}
	if counts["turn/start"] != 1 || counts["step/start"] != 2 || counts["user/message"] != 2 || len(s.Inbox().NextStep) != 0 {
		t.Fatalf("steer crossed turn boundary or remained pending: counts=%v inbox=%+v", counts, s.Inbox())
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeLoopNextStepCanStartIdleTurn(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-idle-inject", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeAnswerModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	message, err := json.Marshal(nativeTextMessage("injected", "user", map[string]any{"kind": "user"}, "injected input"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.InjectMessage(context.Background(), message); err != nil || model.calls != 0 {
		t.Fatalf("inject should only enqueue: %v, calls=%d", err, model.calls)
	}
	if ran, answer, err := loop.RunNext(context.Background()); err != nil || !ran || answer != "answer-1" {
		t.Fatalf("idle next-step turn = %v, %q, %v", ran, answer, err)
	}
	if len(s.Inbox().NextStep) != 0 || len(s.Inbox().NextTurn) != 0 {
		t.Fatalf("injected input not claimed: %+v", s.Inbox())
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeLoopFirstStepClaimsNextStepBeforeNextTurn(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-first-batch", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeHoldingModel{started: make(chan struct{}), release: make(chan struct{})}
	close(model.release)
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	steer, err := json.Marshal(nativeTextMessage("steer-first", "user", map[string]any{"kind": "user"}, "next-step first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.SteerMessage(context.Background(), steer); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.AcceptText(context.Background(), "next-turn second"); err != nil {
		t.Fatal(err)
	}
	if ran, answer, err := loop.RunNext(context.Background()); err != nil || !ran || answer != "answer-1" {
		t.Fatalf("mixed first-step batch = %v, %q, %v", ran, answer, err)
	}
	if len(model.observed) != 1 || len(model.observed[0]) != 3 ||
		model.observed[0][1].Content != "next-step first" || model.observed[0][2].Content != "next-turn second" {
		t.Fatalf("first-step claim order changed: %+v", model.observed)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

type nativeFileCaptureModel struct{ observed []Message }

func (m *nativeFileCaptureModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	m.observed = append([]Message(nil), messages...)
	return session.AssistantMessage{Content: "done"}, nil
}

func TestNativeLoopProjectsDurableFileReferenceToModelHandle(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-file", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeFileCaptureModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	ref := NativeFileAttachmentRef{ID: "sha256:" + strings.Repeat("a", 64), Name: `report "Q3".txt`, Bytes: 8}
	loop.SetFilePathResolver(func(found NativeFileAttachmentRef) string {
		if found != ref {
			t.Fatalf("file resolver received %+v, want %+v", found, ref)
		}
		return "/root/harness/inputs/report.txt"
	})
	message, err := json.Marshal(map[string]any{
		"id": "file-user", "role": "user", "source": map[string]any{"kind": "user"},
		"content": []any{
			map[string]any{"type": "text", "text": "Read the file."},
			map[string]any{"type": "file", "attachment": map[string]any{"attachmentId": ref.ID, "name": ref.Name, "bytes": ref.Bytes}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.AcceptMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if ran, answer, err := loop.RunNext(context.Background()); err != nil || !ran || answer != "done" {
		t.Fatalf("file turn = %v, %q, %v", ran, answer, err)
	}
	if len(model.observed) != 2 || len(model.observed[1].ContentBlocks) != 2 || !model.observed[1].ContentBlocksComplete ||
		!strings.Contains(model.observed[1].Content, `File "report \"Q3\".txt" (8 bytes, sha256:aaaaaaaa)`) ||
		!strings.Contains(model.observed[1].Content, `read-only copy saved at "/root/harness/inputs/report.txt"`) {
		t.Fatalf("file block was not projected as a handle: %+v", model.observed)
	}
	if !strings.Contains(nativeFileHandleText(ref, ""), "cannot access a readable path") {
		t.Fatal("unresolvable attachment incorrectly claims file access")
	}
	var durableFile bool
	for _, event := range s.DurableEvents() {
		if event.Type == "user/message" && strings.Contains(string(event.Data), `"type":"file"`) {
			durableFile = true
		}
	}
	if !durableFile {
		t.Fatal("model projection replaced the durable file reference")
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeLoopAdmitsNextTurnWhileCurrentModelIsRunning(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-overlap", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeHoldingModel{started: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-model.release:
		default:
			close(model.release)
		}
	}()
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := loop.AcceptText(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, _, runErr := loop.RunNext(context.Background())
		firstDone <- runErr
	}()
	select {
	case <-model.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first model request did not start")
	}
	secondDone := make(chan struct {
		id  string
		err error
	}, 1)
	go func() {
		id, acceptErr := loop.AcceptText(context.Background(), "second")
		secondDone <- struct {
			id  string
			err error
		}{id: id, err: acceptErr}
	}()
	var secondID string
	select {
	case result := <-secondDone:
		if result.err != nil {
			t.Fatal(result.err)
		}
		secondID = result.id
	case <-time.After(2 * time.Second):
		t.Fatal("next-turn admission blocked behind the active model request")
	}
	if firstID == secondID || len(s.Inbox().NextTurn) != 1 {
		t.Fatalf("concurrent admission did not persist exactly one next-turn message: %+v", s.Inbox())
	}
	// The first request is still held, so the durable prefix must already
	// contain its claimed user message before the second admission's Flush.
	var firstUser, secondInbox bool
	for _, event := range s.DurableEvents() {
		if event.Type == "user/message" && strings.Contains(string(event.Data), firstID) {
			firstUser = true
		}
		if event.Type == "agent/inbox/spliced" && strings.Contains(string(event.Data), secondID) {
			secondInbox = true
		}
	}
	if !firstUser || !secondInbox {
		t.Fatalf("concurrent durable prefix lost claim or admission: first=%v second=%v", firstUser, secondInbox)
	}
	close(model.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if ran, answer, err := loop.RunNext(context.Background()); err != nil || !ran || answer != "answer-2" {
		t.Fatalf("queued second turn = %v, %q, %v", ran, answer, err)
	}
	if model.calls != 2 || len(s.Inbox().NextTurn) != 0 {
		t.Fatalf("overlapped turns = calls %d, inbox %+v", model.calls, s.Inbox())
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatalf("concurrent native history is invalid: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
