package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go-bot/harness/session"
	"go-bot/internal/modeldata"
)

type nativeMaxTokensModel struct {
	loop  *NativeLoop
	calls int
	seen  [][]Message
}

func (m *nativeMaxTokensModel) Complete(ctx context.Context, messages []Message, _ []ToolSpec) (modeldata.AssistantMessage, error) {
	m.calls++
	m.seen = append(m.seen, append([]Message(nil), messages...))
	if m.calls == 1 {
		steer, err := json.Marshal(nativeTextMessage(uuid.NewString(), "user", map[string]any{"kind": "user"}, "continue partial answer"))
		if err != nil {
			return modeldata.AssistantMessage{}, err
		}
		if _, err := m.loop.SteerMessage(ctx, steer); err != nil {
			return modeldata.AssistantMessage{}, err
		}
		return modeldata.AssistantMessage{
			Content: "partial", MaxTokens: true, Usage: &modeldata.ModelUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			ToolCalls: []modeldata.ToolRequest{{ID: "truncated-call", Name: "echo", Arguments: `{}`}},
			OutputBlocks: []modeldata.AssistantOutputBlock{
				{Type: "text", Text: "partial"},
				{Type: "tool-call", ID: "truncated-call", Name: "echo", Arguments: `{}`},
			},
		}, nil
	}
	return modeldata.AssistantMessage{Content: fmt.Sprintf("answer-%d", m.calls)}, nil
}

func TestNativeMaxTokensKeepsPartialTextSkipsToolsAndSticksToTurn(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-max-tokens", CreatedAt: 1}
	journal := &nativeTestJournal{}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model, tool := &nativeMaxTokensModel{}, &nativeTestTool{}
	loop, err := NewNativeLoop(live, model, "provider", "model", "prompt", []Tool{tool}, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	model.loop = loop
	if answer, err := loop.RunTextTurn(context.Background(), "go"); err != nil || answer != "answer-2" {
		t.Fatalf("first turn: answer=%q err=%v", answer, err)
	}
	if answer, err := loop.RunTextTurn(context.Background(), "new turn"); err != nil || answer != "answer-3" {
		t.Fatalf("second turn: answer=%q err=%v", answer, err)
	}
	if tool.calls != 0 || model.calls != 3 || len(model.seen) != 3 {
		t.Fatalf("truncated tool was dispatched or continuation lost: tools=%d models=%d", tool.calls, model.calls)
	}
	var sawPartial bool
	for _, message := range model.seen[1] {
		sawPartial = sawPartial || message.Role == "assistant" && message.Content == "partial" && len(message.ToolCalls) == 0
	}
	if !sawPartial {
		t.Fatal("follow-up model request did not retain safe partial assistant text")
	}
	var reasons []string
	var maxFinish, toolIntent bool
	for _, event := range live.DurableEvents() {
		switch event.Type {
		case "turn/end":
			var row struct {
				Reason struct {
					Kind string `json:"kind"`
				} `json:"reason"`
			}
			if err := json.Unmarshal(event.Data, &row); err != nil {
				t.Fatal(err)
			}
			reasons = append(reasons, row.Reason.Kind)
		case "assistant/message":
			maxFinish = maxFinish || strings.Contains(string(event.Data), `"kind":"max-tokens"`)
		case "tool/call":
			toolIntent = true
		}
	}
	if len(reasons) != 2 || reasons[0] != "max-tokens" || reasons[1] != "completed" || !maxFinish || toolIntent {
		t.Fatalf("wrong max-token turn history: reasons=%v finish=%v toolIntent=%v", reasons, maxFinish, toolIntent)
	}
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("SERVERAGENTS_V4_MAX_TOKENS_EXPORT_PATH"); path != "" {
		if err := os.WriteFile(path, nativeTestJournalJSONL(t, header, journal), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

type nativeEmptyMaxTokensModel struct{}

func (nativeEmptyMaxTokensModel) Complete(context.Context, []Message, []ToolSpec) (modeldata.AssistantMessage, error) {
	return modeldata.AssistantMessage{MaxTokens: true, ToolCalls: []modeldata.ToolRequest{{ID: "partial-call", Name: "echo", Arguments: `{`}}}, nil
}

func TestNativeMaxTokensCanCommitEmptyCompletionAnchor(t *testing.T) {
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "native-empty-max-tokens", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	tool := &nativeTestTool{}
	loop, err := NewNativeLoop(live, nativeEmptyMaxTokensModel{}, "provider", "model", "prompt", []Tool{tool}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := loop.RunTextTurn(context.Background(), "go"); err != nil || answer != "" || tool.calls != 0 {
		t.Fatalf("empty max-token completion: answer=%q err=%v tools=%d", answer, err, tool.calls)
	}
	var assistant, maxEnd bool
	for _, event := range live.DurableEvents() {
		if event.Type == "assistant/message" && strings.Contains(string(event.Data), `"content":[]`) {
			assistant = true
		}
		if event.Type == "turn/end" && strings.Contains(string(event.Data), `"kind":"max-tokens"`) {
			maxEnd = true
		}
	}
	if !assistant || !maxEnd {
		t.Fatal("max-token step lacked empty assistant anchor or terminal reason")
	}
	messages, err := nativeModelMessages(live, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Role == "assistant" {
			t.Fatal("empty max-token completion anchor leaked into the next model request")
		}
	}
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
}
