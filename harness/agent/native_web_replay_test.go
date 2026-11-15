package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"go-harness/harness/session"
	"go-harness/internal/modeldata"
)

type historicalWebModel struct {
	calls []modeldata.ToolRequest
	seen  []Message
}

func (m *historicalWebModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (modeldata.AssistantMessage, error) {
	m.seen = messages
	calls := m.calls
	m.calls = nil
	return modeldata.AssistantMessage{Content: "done", ToolCalls: calls}, nil
}

type historicalWebTool string

func (t historicalWebTool) Spec() ToolSpec {
	return ToolSpec{Name: string(t), Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (historicalWebTool) Validate(json.RawMessage) error { return nil }
func (t historicalWebTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "saved " + string(t), nil
}

func TestColdReplayPreservesRetiredWebToolNames(t *testing.T) {
	ctx := t.Context()
	header := sessionformat.Header{Type: "session", Version: 4, ID: "legacy-web", CreatedAt: 1, CWD: "/workspace"}
	journal := &nativeTestJournal{}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	old := &historicalWebModel{calls: []modeldata.ToolRequest{
		{ID: "legacy-search", Name: "websearch", Arguments: "{}"},
		{ID: "legacy-fetch", Name: "webextract", Arguments: "{}"},
	}}
	loop, err := NewNativeLoop(live, old, "provider", "model", "prompt",
		[]Tool{historicalWebTool("websearch"), historicalWebTool("webextract")}, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(ctx, "prior conversation"); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	stored, err := sessionformat.DecodePhysicalJSONL(bytes.NewReader(nativeTestJournalJSONL(t, header, journal)))
	if err != nil {
		t.Fatal(err)
	}
	nextJournal := &nativeTestJournal{events: append([]sessionformat.Event(nil), stored.Events...)}
	resumed, err := sessionformat.RestoreLiveSession(stored, nextJournal)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	firstNewEvent := len(stored.Events)
	model := &historicalWebModel{}
	loop, err = NewNativeLoop(resumed, model, "provider", "model", "prompt", nil, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(ctx, "resume old conversation"); err != nil {
		t.Fatal(err)
	}
	results := map[string]string{}
	for _, message := range model.seen {
		if message.Role == "tool" {
			results[message.ToolName] = message.Content
		}
	}
	if len(results) != 2 || results["websearch"] != "saved websearch" || results["webextract"] != "saved webextract" {
		t.Fatalf("historical web tool results lost on resume: %v", results)
	}
	for _, event := range nextJournal.events[firstNewEvent:] {
		if event.Type == "tool/call" || event.Type == "tool/result" {
			t.Fatalf("resume reran a completed tool: %v", event)
		}
	}
}
