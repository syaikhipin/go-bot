package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentcontent "go-bot/harness/content"
	"go-bot/harness/session"
	"go-bot/internal/modeldata"
	"go-bot/thirdparty/llm"
)

type anthropicNonstreamModel struct{ adapter LLMAdapter }

func (m anthropicNonstreamModel) Complete(ctx context.Context, messages []Message, specs []ToolSpec) (modeldata.AssistantMessage, error) {
	return m.adapter.Complete(ctx, messages, specs)
}

func TestAnthropicNativeReplaySurvivesRestoreAndCrossProtocolSwitch(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		calls++
		if calls == 1 {
			_, _ = io.WriteString(w, `{"id":"m1","stop_reason":"tool_use","content":[{"type":"thinking","thinking":"consider","signature":"native-signature"},{"type":"tool_use","id":"call_1","name":"echo","input":{}}],"usage":{}}`)
			return
		}
		if calls == 2 || calls == 3 {
			if !strings.Contains(string(raw), `"signature":"native-signature"`) {
				t.Errorf("signed thinking absent after tool/restart: %s", raw)
			}
		}
		if calls == 5 {
			if strings.Contains(string(raw), "native-signature") {
				t.Error("compaction replayed signed thinking instead of portable context")
			}
			_, _ = io.WriteString(w, `{"stop_reason":"end_turn","content":[{"type":"text","text":"## Primary Request and Intent\nContinue the task."}],"usage":{}}`)
			return
		}
		if calls == 4 {
			if r.URL.Path != "/responses" || strings.Contains(string(raw), "native-signature") || strings.Contains(string(raw), "anthropic_message") {
				t.Error("native data leaked across protocol")
			}
			_, _ = io.WriteString(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"switched"}]}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"m2","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{}}`)
	}))
	defer server.Close()
	client, _ := llm.NewClient(llm.Endpoint{BaseURL: server.URL, APIKey: "k"}, "model", server.Client())
	model := anthropicNonstreamModel{LLMAdapter{Transport: client, Protocol: llm.AnthropicMessages}}
	header := sessionformat.Header{Type: "session", Version: 4, ID: "anthropic-replay", CreatedAt: 1}
	journal := &nativeTestJournal{}
	live, err := sessionformat.NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(live, model, "endpoint", "model", "prompt", []Tool{&nativeTestTool{}}, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithModelRoute(t.Context(), ModelRoute{Provider: "endpoint", Model: "model"})
	if answer, err := loop.RunTextTurn(ctx, "use echo "+strings.Repeat("old-context ", 300)); err != nil || answer != "done" {
		t.Fatalf("tool turn: %s %v", answer, err)
	}
	events := live.DurableEvents()
	reopened := &nativeTestJournal{events: append([]sessionformat.Event(nil), events...)}
	restored, err := sessionformat.RestoreLiveSession(sessionformat.PhysicalArtifact{Header: header, Events: events}, reopened)
	if err != nil {
		t.Fatal(err)
	}
	next, err := NewNativeLoop(restored, model, "endpoint", "model", "prompt", []Tool{&nativeTestTool{}}, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.RunTextTurn(ctx, "continue"); err != nil {
		t.Fatal(err)
	}
	messages, err := nativeModelMessages(restored, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := LLMAdapter{Transport: client, Protocol: llm.Responses}
	if _, err := adapter.Complete(ctx, messages, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("unexpected model calls %d", calls)
	}
	if compacted, err := next.CompactNow(ctx); err != nil || !compacted {
		t.Fatalf("Anthropic compaction: %v %v", compacted, err)
	}
	if err := sessionformat.ValidateV4Relationships(restored.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	// A different Anthropic endpoint/model receives portable history only.
	capture := &capturingLLMTransport{response: &llm.Response{Status: "completed", Output: []llm.Item{{Type: "message", Content: []llm.ContentBlock{{Type: "text", Text: "ok"}}}}}}
	_, err = (LLMAdapter{Transport: capture, Protocol: llm.AnthropicMessages}).Complete(WithModelRoute(ctx, ModelRoute{Provider: "other", Model: "model"}), messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range capture.request.Input {
		if item.Type == "anthropic_message" {
			t.Fatal("replayed native data into another endpoint")
		}
	}
}

func TestAnthropicReplayIsBoundToEndpointAddress(t *testing.T) {
	capture := &capturingLLMTransport{response: &llm.Response{Status: "completed", Output: []llm.Item{{Type: "message", Content: []llm.ContentBlock{{Type: "text", Text: "ok"}}}}}}
	message := Message{Role: "assistant", Content: "portable", ProviderReplay: &modeldata.ProviderReplay{Protocol: string(llm.AnthropicMessages), EndpointID: "same-id", EndpointURL: "https://old.example/v1", ModelID: "model", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"native"}`)}}}
	adapter := LLMAdapter{Transport: capture, Protocol: llm.AnthropicMessages, EndpointURL: "https://new.example/v1"}
	_, err := adapter.Complete(WithModelRoute(t.Context(), ModelRoute{Provider: "same-id", Model: "model"}), []Message{message}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(capture.request.Input) != 1 || capture.request.Input[0].Type != "message" || capture.request.Input[0].Content[0].Text != "portable" {
		t.Fatal("native replay followed an edited endpoint address")
	}
}

func TestAnthropicToolImageStaysInResult(t *testing.T) {
	capture := &capturingLLMTransport{response: &llm.Response{Status: "completed", Output: []llm.Item{{Type: "message", Content: []llm.ContentBlock{{Type: "text", Text: "ok"}}}}}}
	ref := agentcontent.ImageAttachment{ID: "image", MIMEType: "image/png", Bytes: 5, Width: 1, Height: 1}
	adapter := LLMAdapter{Transport: capture, Protocol: llm.AnthropicMessages, Images: &recordingImageReader{data: []byte("image")}, ImageInputEnabled: true}
	_, err := adapter.Complete(t.Context(), []Message{{Role: "tool", ToolCallID: "call-1", IsError: true, ContentBlocks: []agentcontent.ContentBlock{{Type: "text", Text: "result"}, agentcontent.ImageBlock(ref)}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(capture.request.Input) != 1 || capture.request.Input[0].Type != "tool_result" || !capture.request.Input[0].IsError || len(capture.request.Input[0].Content) != 2 || capture.request.Input[0].Content[1].Source == nil {
		t.Fatalf("detached image or error flag: %+v", capture.request.Input)
	}
}
