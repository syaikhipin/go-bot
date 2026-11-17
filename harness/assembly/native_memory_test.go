package assembly

import (
	"context"
	"strings"
	"testing"

	"go-bot/harness/agent"
	"go-bot/harness/memory"
	"go-bot/internal/testsupport/session"
)

const rememberedPreference = "日报用中文"

type memoryTurnModel struct {
	requests [][]agent.Message
}

func (m *memoryTurnModel) Complete(_ context.Context, messages []agent.Message, specs []agent.ToolSpec) (session.AssistantMessage, error) {
	m.requests = append(m.requests, messages)
	if len(m.requests) == 1 {
		return session.AssistantMessage{ToolCalls: []session.ToolRequest{{
			ID: "remember", Name: "memory",
			Arguments: `{"target":"user","operations":[{"action":"add","content":"日报用中文"}]}`,
		}}}, nil
	}
	return session.AssistantMessage{Content: "done"}, nil
}

func (m *memoryTurnModel) Summarize(context.Context, []agent.Message, []agent.ToolSpec) (agent.SummaryResult, error) {
	return agent.SummaryResult{Text: "Previous work was discussed."}, nil
}

func TestNativeMemoryUpdatesWithinTurnAndSurvivesCompaction(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := memory.NewStore(dir)
	model := &memoryTurnModel{}
	factory := NativeFactory{
		Storage: &nativeTreeStorage{nativeMemoryStorage: &nativeMemoryStorage{}}, Builder: nativeTestBuilder{newContainerTestBuilder(t, t.TempDir())},
		Model: model, Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help with the user's task.",
		Limits: agent.Limits{MaxSteps: 4, MaxToolCalls: 4}, Memory: store,
	}
	identity := NativeRuntimeIdentity{UserID: "alice", SessionID: "native-memory", Workspace: "/workspace"}
	live, err := factory.Activate(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	if _, err := live.Agent.RunTextTurn(ctx, "Remember my report preference. "+strings.Repeat("Report context to compact. ", 100)); err != nil {
		t.Fatal(err)
	}
	hasSystemText := func(request int, text string) bool {
		for _, message := range model.requests[request] {
			if message.Role == "system" && strings.Contains(message.Content, text) {
				return true
			}
		}
		return false
	}
	if len(model.requests) != 2 || hasSystemText(0, rememberedPreference) || !hasSystemText(1, rememberedPreference) {
		t.Fatalf("preference did not appear in the second model step: %+v", model.requests)
	}
	if !hasSystemText(0, "You have persistent memory across sessions") || !hasSystemText(0, "The memory tool schema defines") {
		t.Fatal("initial model request did not include the persistent memory guidance")
	}
	if _, err := live.Agent.RunTextTurn(ctx, "Discuss next week's work."); err != nil {
		t.Fatal(err)
	}
	if compacted, err := live.Agent.CompactNow(ctx); err != nil || !compacted {
		t.Fatalf("compact = %t, %v", compacted, err)
	}
	if _, err := live.Agent.RunTextTurn(ctx, "Continue after compaction."); err != nil {
		t.Fatal(err)
	}
	last := len(model.requests) - 1
	if !hasSystemText(last, rememberedPreference) {
		t.Fatal("compaction lost current memory")
	}
	for _, message := range model.requests[last] {
		if message.Role == "tool" && message.ToolCallID == "remember" {
			t.Fatal("test did not compact away the original memory result")
		}
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	factory.Memory = memory.NewStore(dir)
	if _, err := factory.Memory.Apply("user", []memory.Operation{{Action: "remove", OldText: rememberedPreference}}); err != nil {
		t.Fatal(err)
	}
	resumed, err := factory.Resume(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if _, err := resumed.Agent.RunTextTurn(ctx, "Check my preferences."); err != nil {
		t.Fatal(err)
	}
	if hasSystemText(len(model.requests)-1, rememberedPreference) {
		t.Fatal("resumed session retained the deleted memory")
	}
	var systemEvents int
	for _, event := range resumed.Session.DurableEvents() {
		if event.Type == "system/message" {
			systemEvents++
		}
	}
	if systemEvents != 3 {
		t.Fatalf("system events = %d, want initial, write and removal", systemEvents)
	}
	if _, err := factory.Memory.Apply("memory", []memory.Operation{{Action: "add", Content: "Use the new archive directory."}}); err != nil {
		t.Fatal(err)
	}
	fork, err := factory.ForkChild(ctx, identity, resumed, "native-memory-fork", "Check saved notes")
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	if _, err := fork.Agent.RunTextTurn(ctx, "Use the saved archive location."); err != nil {
		t.Fatal(err)
	}
	if !hasSystemText(len(model.requests)-1, "Use the new archive directory.") {
		t.Fatal("fork did not read the current memory after inheriting the parent's history")
	}
}
