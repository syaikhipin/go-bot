package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAnthropicRequestGroupsToolsAndKeepsImages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "secret" || r.Header.Get("Authorization") != "" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("wrong Anthropic routing/auth: %s", r.URL.Path)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["max_tokens"]) != "16384" || len(body["reasoning"]) != 0 || len(body["thinking"]) != 0 {
			t.Error("invalid default parameters")
		}
		var messages []struct {
			Role    string                       `json:"role"`
			Content []map[string]json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(body["messages"], &messages)
		if len(messages) != 3 || messages[1].Role != "assistant" || len(messages[1].Content) != 3 || messages[2].Role != "user" || len(messages[2].Content) != 3 {
			t.Fatalf("incorrect grouping: %s", body["messages"])
		}
		if string(messages[2].Content[0]["tool_use_id"]) != `"call_a"` || string(messages[2].Content[1]["tool_use_id"]) != `"call_b"` || string(messages[2].Content[0]["is_error"]) != "true" {
			t.Fatal("tool results or steering were reordered incorrectly")
		}
		var parts []map[string]json.RawMessage
		_ = json.Unmarshal(messages[2].Content[0]["content"], &parts)
		if len(parts) != 2 || !strings.Contains(string(parts[1]["source"]), `"base64"`) {
			t.Fatal("tool image was detached")
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":10,"cache_read_input_tokens":4,"cache_creation_input_tokens":3,"output_tokens":5}}`)
	}))
	defer server.Close()
	client, _ := NewClient(Endpoint{BaseURL: server.URL + "/v1", APIKey: "secret"}, "custom-model", server.Client())
	result, err := client.GenerateWithProtocol(t.Context(), AnthropicMessages, Request{Input: []Item{
		{Type: "message", Role: "system", Content: []ContentBlock{{Type: "text", Text: "system"}}},
		{Type: "message", Role: "user", Content: []ContentBlock{{Type: "text", Text: "run both"}}},
		{Type: "message", Role: "assistant", Content: []ContentBlock{{Type: "text", Text: "working"}}},
		{Type: "tool_call", CallID: "call_a", Name: "a", Arguments: `{}`}, {Type: "tool_call", CallID: "call_b", Name: "b", Arguments: `{}`},
		{Type: "tool_result", CallID: "call_a", IsError: true, Content: []ContentBlock{{Type: "text", Text: "image"}, {Type: "image", Source: &ImageSource{Type: "url", URL: "data:image/png;base64,aGVsbG8="}}}},
		{Type: "message", Role: "user", Content: []ContentBlock{{Type: "text", Text: "steering"}}},
		{Type: "tool_result", CallID: "call_b", Content: []ContentBlock{{Type: "text", Text: "ok"}}},
	}, Tools: []Tool{{Name: "a", Parameters: json.RawMessage(`{"type":"object"}`)}}, ReasoningEffort: "medium"})
	if err != nil || result.Output[0].Content[0].Text != "done" || result.Usage.InputTokens != 17 || result.Usage.TotalTokens != 22 || result.Usage.CacheCreationTokens != 3 {
		t.Fatalf("response: %+v %v", result, err)
	}
}

func anthropicFrame(kind string, fields string) string {
	if fields != "" {
		fields = "," + fields
	}
	return fmt.Sprintf("event: %s\ndata: {\"type\":%q%s}\n\n", kind, kind, fields)
}
func anthropicToolStream() string {
	return anthropicFrame("message_start", `"message":{"id":"m","usage":{"input_tokens":10,"cache_read_input_tokens":2}}`) +
		anthropicFrame("content_block_start", `"index":0,"content_block":{"type":"thinking","thinking":"","signature":""}`) +
		anthropicFrame("content_block_delta", `"index":0,"delta":{"type":"thinking_delta","thinking":"consider"}`) +
		anthropicFrame("content_block_delta", `"index":0,"delta":{"type":"signature_delta","signature":"signed"}`) +
		anthropicFrame("content_block_stop", `"index":0`) +
		anthropicFrame("content_block_start", `"index":1,"content_block":{"type":"text","text":""}`) +
		anthropicFrame("content_block_delta", `"index":1,"delta":{"type":"text_delta","text":"hello"}`) +
		anthropicFrame("content_block_stop", `"index":1`) +
		anthropicFrame("content_block_start", `"index":2,"content_block":{"type":"tool_use","id":"t1","name":"echo","input":{}}`) +
		anthropicFrame("content_block_delta", `"index":2,"delta":{"type":"input_json_delta","partial_json":"{\"value\":"}`) +
		anthropicFrame("content_block_delta", `"index":2,"delta":{"type":"input_json_delta","partial_json":"\"ok\"}"}`) +
		anthropicFrame("content_block_stop", `"index":2`) +
		anthropicFrame("message_delta", `"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5,"cache_creation_input_tokens":3}`) +
		anthropicFrame("message_stop", "")
}
func TestAnthropicStreamingAndNativeReplay(t *testing.T) {
	var events []StreamEvent
	response, err := consumeAnthropicStream(strings.NewReader(anthropicToolStream()), func(e StreamEvent) error { events = append(events, e); return nil }, nil)
	if err != nil || len(response.Output) != 3 || response.Output[2].Arguments != `{"value":"ok"}` || response.Usage.InputTokens != 15 || response.Usage.OutputTokens != 5 {
		t.Fatalf("stream response %+v, %v", response, err)
	}
	if !strings.Contains(string(response.NativeContent[0]), `"signature":"signed"`) || events[1].Type != "reasoning.delta" || events[len(events)-1].Type != "completed" {
		t.Fatal("lost signed thinking or live deltas")
	}
	raw, _ := json.Marshal(response.NativeContent)
	body, err := encodeAnthropic("m", Request{Input: []Item{{Type: "message", Role: "user", Content: []ContentBlock{{Type: "text", Text: "do it"}}}, {Type: "anthropic_message", Raw: raw}, {Type: "tool_result", CallID: "t1", Content: []ContentBlock{{Type: "text", Text: "done"}}}}}, true)
	if err != nil || !strings.Contains(string(body), `"signature":"signed"`) {
		t.Fatalf("native replay: %s %v", body, err)
	}
}
func TestAnthropicRejectsIncompleteOrFailedStreams(t *testing.T) {
	full := anthropicToolStream()
	cases := []string{strings.TrimSuffix(full, anthropicFrame("message_stop", "")), strings.TrimSuffix(full, "\n\n"), anthropicFrame("error", `"error":{"type":"overloaded_error","message":"busy"}`), anthropicFrame("content_block_delta", `"index":0,"delta":{"type":"text_delta","text":"bad"}`), strings.Replace(full, `\"ok\"}`, `oops`, 1)}
	for i, stream := range cases {
		completed := false
		_, err := consumeAnthropicStream(strings.NewReader(stream), func(e StreamEvent) error { completed = completed || e.Type == "completed"; return nil }, nil)
		if err == nil || completed {
			t.Errorf("case %d accepted corrupt stream: %v", i, err)
		}
	}
}
func TestAnthropicStopReasonsAndErrors(t *testing.T) {
	for _, reason := range []string{"max_tokens", "refusal", "pause_turn", "model_context_window_exceeded"} {
		response, err := decodeAnthropic([]byte(fmt.Sprintf(`{"id":"m","stop_reason":%q,"content":[{"type":"text","text":"partial"}],"usage":{}}`, reason)))
		if err == nil || response == nil || response.Status == "completed" {
			t.Fatalf("unsafe stop reason %s", reason)
		}
	}
	err := decodeAPIError(401, []byte(`{"error":{"type":"authentication_error","message":"invalid key"}}`))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "authentication_error" {
		t.Fatal(err)
	}
	_, err = encodeAnthropic("m", Request{Input: []Item{{Type: "tool_call", CallID: "orphan", Name: "echo", Arguments: `{}`}}}, false)
	if err == nil {
		t.Fatal("unpaired call accepted")
	}
	_, err = decodeAnthropic([]byte(`{"stop_reason":"end_turn","content":[],"usage":{"input_tokens":9223372036854775807,"output_tokens":1}}`))
	if err == nil {
		t.Fatal("overflowed usage accepted")
	}
}
func TestAnthropicStreamingCancellationAndLiveDelta(t *testing.T) {
	entered := make(chan struct{})
	delta := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicFrame("message_start", `"message":{"id":"m","usage":{}}`)+anthropicFrame("content_block_start", `"index":0,"content_block":{"type":"text","text":""}`)+anthropicFrame("content_block_delta", `"index":0,"delta":{"type":"text_delta","text":"live"}`))
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := NewClient(Endpoint{BaseURL: server.URL, APIKey: "k"}, "m", server.Client())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.StreamWithProtocol(ctx, AnthropicMessages, Request{Input: []Item{{Type: "message", Role: "user", Content: []ContentBlock{{Type: "text", Text: "hello"}}}}}, func(e StreamEvent) error {
			if e.Type == "message.delta" {
				delta <- struct{}{}
			}
			return nil
		})
		done <- err
	}()
	<-entered
	select {
	case <-delta:
	case <-time.After(time.Second):
		t.Fatal("buffered live delta")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not close stream")
	}
}

func TestAnthropicDoesNotForwardSecretsOnRedirect(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(200) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client, _ := NewClient(Endpoint{BaseURL: origin.URL, APIKey: "private-key"}, "model", origin.Client())
	_, err := client.GenerateWithProtocol(t.Context(), AnthropicMessages, Request{Input: []Item{{Type: "message", Role: "user", Content: []ContentBlock{{Type: "text", Text: "hello"}}}}})
	if err == nil || reached {
		t.Fatal("model credentials followed redirect")
	}
}
