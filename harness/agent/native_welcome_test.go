package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	sessionformat "go-bot/harness/session"
	modeldata "go-bot/internal/testsupport/session"
)

type welcomeContextModel struct {
	t     *testing.T
	calls int
}

func (m *welcomeContextModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (modeldata.AssistantMessage, error) {
	m.calls++
	if len(messages) < 3 || messages[0].Role != "system" || messages[1].Role != "assistant" || messages[1].Content != "Welcome" {
		m.t.Fatalf("context order: %+v", messages)
	}
	if !strings.Contains(messages[0].Content, "current prompt") {
		m.t.Fatal("initial system was not refreshed")
	}
	return modeldata.AssistantMessage{Content: "OK"}, nil
}

func TestWelcomeIsAssistantAfterSystemAcrossStepsAndResume(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "main-welcome", CreatedAt: 1, CWD: "/workspace"}
	seed := []sessionformat.Event{
		{Type: "system/message", Seq: 0, Time: 1, SurfaceOp: json.RawMessage(`"append"`), Data: json.RawMessage(`{"message":{"id":"initial-system","role":"system","source":{"kind":"go-bot/initial-system"},"content":[{"type":"text","text":"initial prompt"}]}}`)},
		{Type: "assistant/message", Seq: 1, Time: 1, SurfaceOp: json.RawMessage(`"append"`), Data: json.RawMessage(`{"stream":[],"message":{"id":"welcome","role":"assistant","source":{"kind":"go-bot/welcome"},"content":[{"type":"text","text":"Welcome"}]}}`)},
	}
	prepared, err := sessionformat.PrepareSession(header, seed, nil)
	if err != nil {
		t.Fatal(err)
	}
	journal := &nativeTestJournal{events: prepared.Events()}
	live, err := prepared.Attach(journal, len(journal.events))
	if err != nil {
		t.Fatal(err)
	}
	model := &welcomeContextModel{t: t}
	run := func(live *sessionformat.LiveSession) {
		loop, err := NewNativeLoop(live, model, "provider", "model", "current prompt", nil, 2, 2)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := loop.AcceptText(t.Context(), strings.Repeat("hello ", 200)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loop.RunNext(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	run(live)
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	artifact, err := sessionformat.DecodePhysicalJSONL(bytes.NewReader(nativeTestJournalJSONL(t, header, journal)))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := sessionformat.RestoreLiveSession(artifact, journal)
	if err != nil {
		t.Fatal(err)
	}
	run(restored)
	if model.calls != 2 {
		t.Fatal(model.calls)
	}
	count := 0
	for _, e := range restored.DurableEvents() {
		if e.Type == "assistant/message" && strings.Contains(string(e.Data), `"go-bot/welcome"`) {
			count++
		}
	}
	if count != 1 {
		t.Fatal("welcome duplicated", count)
	}
	usage, err := sessionformat.ProjectUsage(restored.DurableEvents())
	if err != nil {
		t.Fatal(err)
	}
	if usage.AssistantMessages != 2 || usage.StepsStarted != 2 {
		t.Fatalf("onboarding counted as model execution: %+v", usage)
	}
	compactModel := &nativeCompactionModel{}
	compactLoop, err := NewNativeLoop(restored, compactModel, "provider", "model", "current prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if compacted, err := compactLoop.CompactNow(t.Context()); err != nil || !compacted {
		t.Fatalf("compact welcome history: %v %v", compacted, err)
	}
	if err := sessionformat.ValidateV4Relationships(restored.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	surface, err := restored.Surface()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(surface.Messages[0]), "Welcome") || !strings.Contains(string(surface.Messages[0]), "current prompt") {
		t.Fatal("compaction lost system head")
	}
	for _, raw := range surface.Messages[1:] {
		if strings.Contains(string(raw), `"id":"welcome"`) {
			t.Fatal("welcome was pinned outside ordinary compaction")
		}
	}
	if _, err := sessionformat.DecodePhysicalJSONL(bytes.NewReader(nativeTestJournalJSONL(t, header, journal))); err != nil {
		t.Fatal(err)
	}
}
