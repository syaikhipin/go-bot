package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sessionformat "go-bot/harness/session"
)

func TestPromptAssemblyPinsRealHandlersAndCanRestoreDeclarationsNextStep(t *testing.T) {
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, CreatedAt: 1, ID: "assembly", CWD: "/tmp"}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	probe := &dynamicProbe{label: "real"}
	loop, err := NewNativeLoop(live, &nativeAnswerModel{}, "provider", "model", "base", []Tool{probe}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	hidden := true
	loop.SetPromptAssembler(func(_ context.Context, input PromptAssemblyInput) (PromptAssembly, error) {
		if input.SessionID != "assembly" || input.CWD != "/tmp" || input.SystemPrompt != "base" || len(input.Tools) != 1 {
			t.Errorf("incomplete input %+v", input)
		}
		tools := input.Tools
		if hidden {
			tools = []ToolSpec{}
		}
		return PromptAssembly{SystemPrompt: "assembled policy", Contexts: []RuntimeContextSection{{Name: "state", Text: "registered context"}}, Tools: tools}, nil
	})
	if err = loop.refreshTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = loop.ensureSystem(t.Context(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if len(loop.tools) != 0 {
		t.Fatal("hidden declarations were still dispatchable")
	}
	if err = loop.ensureRuntimeContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live.Events()[0].Data), "assembled policy") || !strings.Contains(string(live.Events()[1].Data), "registered context") {
		t.Fatal("assembled prompt/context were not committed through existing logs")
	}
	hidden = false
	if err = loop.refreshTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = loop.ensureSystem(t.Context(), 1, 2); err != nil {
		t.Fatal(err)
	}
	if loop.tools[probe.Spec().Name] != probe {
		t.Fatal("assembly lost original Go handler")
	}
	loop.SetPromptAssembler(func(context.Context, PromptAssemblyInput) (PromptAssembly, error) {
		return PromptAssembly{Tools: []ToolSpec{{Name: "invented", Parameters: json.RawMessage(`{"type":"object"}`)}}}, nil
	})
	if err = loop.ensureSystem(t.Context(), 1, 3); err == nil || loop.tools[probe.Spec().Name] != probe {
		t.Fatalf("unknown tool changed pinned table: %v", err)
	}
	loop.SetPromptAssembler(func(context.Context, PromptAssemblyInput) (PromptAssembly, error) {
		return PromptAssembly{}, errors.New("provider unavailable")
	})
	if err = loop.ensureSystem(t.Context(), 1, 3); err == nil {
		t.Fatal("prompt failure was hidden")
	}
}
