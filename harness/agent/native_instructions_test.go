package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"go-harness/harness/session"
	"go-harness/internal/testsupport/session"
)

type nativeInstructionModel struct{ observed [][]Message }

func (m *nativeInstructionModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	m.observed = append(m.observed, append([]Message(nil), messages...))
	return session.AssistantMessage{Content: "done"}, nil
}

type nativeInstructionTouchModel struct{ calls int }

func (m *nativeInstructionTouchModel) Complete(_ context.Context, _ []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	if m.calls == 1 {
		return session.AssistantMessage{ToolCalls: []session.ToolRequest{{ID: "read-1", Name: "readfile", Arguments: `{"path":"src/package/main.go"}`}}}, nil
	}
	return session.AssistantMessage{Content: "done"}, nil
}

type nativeInstructionReadTool struct{ nativeTestTool }

func (*nativeInstructionReadTool) Spec() ToolSpec {
	return ToolSpec{Name: "readfile", Description: "Read test file", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func TestNativeInstructionsTrackSuccessfulFileToolTouches(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "instructions-touch", CreatedAt: 1, CWD: "/workspace"}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeInstructionTouchModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "system", []Tool{&nativeInstructionReadTool{}}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	var seen [][]string
	loop.SetInstructionProvider(func(_ context.Context, touched []string) (NativeInstructionSnapshot, error) {
		seen = append(seen, append([]string(nil), touched...))
		return NativeInstructionSnapshot{Identity: "one"}, nil
	})
	if _, err := loop.RunTextTurn(context.Background(), "read nested file"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || len(seen[0]) != 0 || len(seen[1]) != 1 || seen[1][0] != "src/package/main.go" {
		t.Fatalf("file touch not carried into next step: %#v", seen)
	}
}

func TestNativeInstructionsFollowUserAndChangeWithoutRepeatingUnchangedBaseline(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "instructions", CreatedAt: 1, CWD: "/workspace"}
	journal := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeInstructionModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "system", nil, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	current := NativeInstructionSnapshot{
		Identity: "one-project", Text: "<system-reminder>\nInstructions from: AGENTS.md\n\nfirst\n</system-reminder>",
		Changes: []NativeInstructionChange{{Action: "set", Scope: ".\x00AGENTS.md", Path: "AGENTS.md", Digest: "first"}},
		Content: map[string]string{".\x00AGENTS.md": "first"},
	}
	loop.SetInstructionProvider(func(context.Context, []string) (NativeInstructionSnapshot, error) { return current, nil })
	for _, content := range []string{"first request", "same baseline"} {
		if _, err := loop.RunTextTurn(context.Background(), content); err != nil {
			t.Fatal(err)
		}
	}
	current.Text = "<system-reminder>\nInstructions from: AGENTS.md\n\nsecond\n</system-reminder>"
	current.Changes[0].Digest = "second"
	current.Content[".\x00AGENTS.md"] = "second"
	if _, err := loop.RunTextTurn(context.Background(), "after file edit"); err != nil {
		t.Fatal(err)
	}
	current.Text = ""
	current.Changes = nil
	current.Content = nil
	if _, err := loop.RunTextTurn(context.Background(), "after file removal"); err != nil {
		t.Fatal(err)
	}
	var instructionMessages []string
	var sourceActions []string
	var sourceBaselines []bool
	for _, event := range s.DurableEvents() {
		if event.Type != "user/message" {
			continue
		}
		var row struct {
			Source struct {
				Kind     string                    `json:"kind"`
				Baseline bool                      `json:"baseline"`
				Changes  []NativeInstructionChange `json:"changes"`
			} `json:"source"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(event.Data, &row); err != nil {
			t.Fatal(err)
		}
		if row.Source.Kind == "agent-instructions" {
			instructionMessages = append(instructionMessages, row.Content[0].Text)
			sourceBaselines = append(sourceBaselines, row.Source.Baseline)
			if len(row.Source.Changes) != 1 {
				t.Fatalf("instruction source change count = %+v", row.Source.Changes)
			}
			sourceActions = append(sourceActions, row.Source.Changes[0].Action)
		}
	}
	if len(instructionMessages) != 3 || !strings.Contains(instructionMessages[0], "first") ||
		!strings.Contains(instructionMessages[1], "Updated instructions from: AGENTS.md") ||
		!strings.Contains(instructionMessages[1], "second") ||
		!strings.Contains(instructionMessages[2], "Instructions removed: AGENTS.md") {
		t.Fatalf("workspace instruction transitions = %#v", instructionMessages)
	}
	if strings.Join(sourceActions, ",") != "set,replace,remove" ||
		len(sourceBaselines) != 3 || !sourceBaselines[0] || sourceBaselines[1] || sourceBaselines[2] {
		t.Fatalf("instruction source transitions = %v, baselines = %v", sourceActions, sourceBaselines)
	}
	if len(model.observed) != 4 || len(model.observed[0]) != 3 ||
		model.observed[0][1].Content != "first request" ||
		!strings.Contains(model.observed[0][2].Content, "first") {
		t.Fatalf("instruction was not placed after the direct user request: %+v", model.observed)
	}
	if err := sessionformat.ValidateV4Relationships(s.DurableEvents()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeInstructionsDoNotRemoveLastGoodScopeOnUnavailableProbe(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "instructions-unavailable", CreatedAt: 1, CWD: "/workspace"}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeInstructionModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "system", nil, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	current := NativeInstructionSnapshot{
		Identity: "same", Text: "<system-reminder>\nInstructions from: AGENTS.md\n\nkeep this\n</system-reminder>",
		Changes: []NativeInstructionChange{{Action: "set", Scope: ".\x00AGENTS.md", Path: "AGENTS.md", Digest: "old"}},
		Content: map[string]string{".\x00AGENTS.md": "keep this"},
	}
	loop.SetInstructionProvider(func(context.Context, []string) (NativeInstructionSnapshot, error) { return current, nil })
	if _, err := loop.RunTextTurn(context.Background(), "initial"); err != nil {
		t.Fatal(err)
	}
	current = NativeInstructionSnapshot{
		Identity: "same", Text: "<system-reminder>\nInstructions from: AGENTS.md\n\nunconfirmed new text\n</system-reminder>",
		Changes:           []NativeInstructionChange{{Action: "set", Scope: ".\x00AGENTS.md", Path: "AGENTS.md", Digest: "unconfirmed"}},
		Content:           map[string]string{".\x00AGENTS.md": "unconfirmed new text"},
		UnavailableGroups: map[string]bool{".": true},
	}
	if _, err := loop.RunTextTurn(context.Background(), "probe unavailable"); err != nil {
		t.Fatal(err)
	}
	var instructionRows int
	for _, event := range s.DurableEvents() {
		if event.Type == "user/message" && strings.Contains(string(event.Data), `"kind":"agent-instructions"`) {
			instructionRows++
		}
	}
	if instructionRows != 1 || len(model.observed) != 2 || !strings.Contains(model.observed[1][2].Content, "keep this") {
		t.Fatalf("temporary probe failure removed the last-good instruction: rows=%d messages=%+v", instructionRows, model.observed)
	}
	current = NativeInstructionSnapshot{Identity: "same"}
	if _, err := loop.RunTextTurn(context.Background(), "confirmed removal"); err != nil {
		t.Fatal(err)
	}
	instructionRows = 0
	for _, event := range s.DurableEvents() {
		if event.Type == "user/message" && strings.Contains(string(event.Data), `"kind":"agent-instructions"`) {
			instructionRows++
			if instructionRows == 2 && !strings.Contains(string(event.Data), `"action":"remove"`) {
				t.Fatalf("confirmed deletion did not emit a remove transition: %s", event.Data)
			}
		}
	}
	if instructionRows != 2 {
		t.Fatalf("confirmed deletion instruction row count = %d", instructionRows)
	}
}

func TestNativeInstructionDeltaRetainsOnlyChangesRepresentedWithinBudget(t *testing.T) {
	changes := []NativeInstructionChange{
		{Action: "set", Scope: ".\x00AGENTS.md", Path: "AGENTS.md", Digest: "one"},
		{Action: "set", Scope: "nested\x00AGENTS.md", Path: "nested/AGENTS.md", Digest: "two"},
	}
	content := map[string]string{changes[0].Scope: strings.Repeat("界", nativeInstructionDeltaMaxBytes), changes[1].Scope: "specific guidance"}
	text, retained := renderNativeInstructionDelta(changes, content)
	if len(text) > nativeInstructionDeltaMaxBytes || !utf8.ValidString(text) || len(retained) != 1 ||
		retained[0].Scope != changes[1].Scope || !strings.Contains(text, "specific guidance") {
		t.Fatalf("delta byte-budget selection = bytes=%d, retained=%+v", len(text), retained)
	}
	text, retained = renderNativeInstructionDelta(changes[:1], content)
	if len(text) > nativeInstructionDeltaMaxBytes || !utf8.ValidString(text) || len(retained) != 1 ||
		!strings.Contains(text, "Updated instructions") && !strings.Contains(text, "Additional instructions") {
		t.Fatalf("most-specific delta truncation = bytes=%d, retained=%+v", len(text), retained)
	}
}
