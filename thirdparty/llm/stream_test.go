package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestServerAgentStreamIdleDeadlineRenewsOnProviderEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"a"}`,
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"b"}`,
			`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			w.(http.Flusher).Flush()
			time.Sleep(120 * time.Millisecond)
		}
	}))
	defer server.Close()
	client, err := NewClientWithNonStreamingIdleTimeout(Endpoint{BaseURL: server.URL, APIKey: "secret"}, "model", 220*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Stream(context.Background(), Request{}, nil)
	if err != nil || response == nil || len(response.Output) != 1 || response.Output[0].Content[0].Text != "ab" {
		t.Fatalf("progressive stream = %+v, %v", response, err)
	}
}

func TestServerAgentStreamIdleDeadlineStopsStalledProvider(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() { close(release); server.Close() }()
	client, err := NewClientWithNonStreamingIdleTimeout(Endpoint{BaseURL: server.URL, APIKey: "secret"}, "model", 70*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Stream(context.Background(), Request{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("stalled stream = %v", err)
	}
}

func TestServerAgentStreamIdleDeadlineRenewsOnHeartbeatsAndIgnoredFrames(t *testing.T) {
	for _, protocol := range []Protocol{Responses, ChatCompletions} {
		for _, activity := range []string{"comment", "ignored-frame"} {
			t.Run(string(protocol)+"/"+activity, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
					for i := 0; i < 6; i++ {
						time.Sleep(60 * time.Millisecond)
						if activity == "comment" {
							_, _ = io.WriteString(w, ": keep-alive\n\n")
						} else if protocol == Responses {
							_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
						} else {
							_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\n")
						}
						w.(http.Flusher).Flush()
					}
					if protocol == Responses {
						_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n")
					} else {
						_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
					}
				}))
				defer server.Close()
				client, err := NewClientWithNonStreamingIdleTimeout(Endpoint{BaseURL: server.URL, APIKey: "secret"}, "model", 190*time.Millisecond)
				if err != nil {
					t.Fatal(err)
				}
				response, err := client.StreamWithProtocol(context.Background(), protocol, Request{}, nil)
				if err != nil || response == nil || len(response.Output) != 1 || response.Output[0].Content[0].Text != "ok" {
					t.Fatalf("stream after %s activity = %+v, %v", activity, response, err)
				}
			})
		}
	}
}

func TestStreamIgnoresUnterminatedTerminalFrame(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol Protocol
		payload  string
	}{
		{"responses", Responses, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n"},
		{"chat", ChatCompletions, "data: [DONE]\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.payload)
			}))
			defer server.Close()
			client, err := NewClient(Endpoint{BaseURL: server.URL, APIKey: "secret"}, "model", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.StreamWithProtocol(context.Background(), test.protocol, Request{}, nil)
			if err == nil || !strings.Contains(err.Error(), "without") {
				t.Fatalf("unterminated terminal frame was accepted: %v", err)
			}
		})
	}
}

func TestStreamErrorFramePreservesObservedOutputAndUpstreamCode(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol Protocol
		partial  string
		failure  string
	}{
		{"responses", Responses,
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}` + "\n\n",
			`data: {"type":"error","code":"rate_limit_exceeded","message":"too many requests"}` + "\n\n"},
		{"responses-terminal-failure", Responses,
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}` + "\n\n",
			`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"too many requests"},"output":[]}}` + "\n\n"},
		{"chat", ChatCompletions,
			`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n",
			`data: {"error":{"code":"rate_limit_exceeded","message":"too many requests"}}` + "\n\n"},
	} {
		for _, withPartial := range []bool{false, true} {
			name := test.name + "/no-output"
			if withPartial {
				name = test.name + "/observed-output"
			}
			t.Run(name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if withPartial {
						_, _ = io.WriteString(w, test.partial)
					}
					_, _ = io.WriteString(w, test.failure)
				}))
				defer server.Close()
				client, err := NewClient(Endpoint{BaseURL: server.URL, APIKey: "secret"}, "model", server.Client())
				if err != nil {
					t.Fatal(err)
				}
				response, err := client.StreamWithProtocol(context.Background(), test.protocol, Request{}, nil)
				var upstream *APIError
				if !errors.As(err, &upstream) || upstream.Code != "rate_limit_exceeded" {
					t.Fatalf("stream error = %v", err)
				}
				if withPartial {
					if response == nil || len(response.Output) != 1 || response.Output[0].Content[0].Text != "partial" {
						t.Fatalf("observed stream output was lost: %+v", response)
					}
				} else if response != nil {
					t.Fatalf("stream without output returned response: %+v", response)
				}
				if classified := NormalizeModelError(server.URL, "model", err); classified.Code != ModelErrorUpstreamRateLimited {
					t.Fatalf("stream error classification = %+v", classified)
				}
			})
		}
	}
}

func TestStreamRetainsMaxTokenPartialResponseForHarnessSettlement(t *testing.T) {
	for _, test := range []struct {
		name, payload, status, reason string
		protocol                      Protocol
	}{
		{"responses", `data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}],"usage":{"input_tokens":3,"output_tokens":4}}}` + "\n\n", "incomplete", "max_output_tokens", Responses},
		{"chat", `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n" + `data: {"choices":[{"index":0,"finish_reason":"length","delta":{}}]}` + "\n\n" + "data: [DONE]\n\n", "length", "length", ChatCompletions},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.payload)
			}))
			defer server.Close()
			client, err := NewClient(Endpoint{BaseURL: server.URL, APIKey: "secret"}, "model", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			var terminal *Response
			response, err := client.StreamWithProtocol(context.Background(), test.protocol, Request{}, func(event StreamEvent) error {
				if event.Response != nil {
					terminal = event.Response
				}
				return nil
			})
			if err == nil || response == nil || terminal == nil || response.Status != test.status || response.FinishReason != test.reason ||
				len(response.Output) != 1 || len(response.Output[0].Content) != 1 || response.Output[0].Content[0].Text != "partial" ||
				terminal.Status != test.status {
				t.Fatalf("truncated stream lost final facts: response=%+v terminal=%+v err=%v", response, terminal, err)
			}
		})
	}
}

func TestResponsesStreamKeepsOutputIndexOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc-1","delta":"{\"q\":"}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg-1","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","output_index":0,"item_id":"msg-1","content_index":0,"delta":"你好"}`,
			`{"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc-1","arguments":"{\"q\":1}"}`,
			`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":3}}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer server.Close()
	client, err := NewClient(Endpoint{BaseURL: server.URL, APIKey: "secret"}, "qwen", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	response, err := client.Stream(context.Background(), Request{Input: []Item{{Type: "message", Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}}}, func(event StreamEvent) error {
		if event.Delta != "" {
			deltas = append(deltas, event.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{response.Output[0].Type, response.Output[1].Type}; !reflect.DeepEqual(got, []string{"message", "tool_call"}) {
		t.Fatalf("order: %v", got)
	}
	if response.Output[0].Content[0].Text != "你好" || response.Output[1].Arguments != `{"q":1}` {
		t.Fatalf("stream output: %#v", response.Output)
	}
	if !reflect.DeepEqual(deltas, []string{`{"q":`, "你好"}) {
		t.Fatalf("deltas: %v", deltas)
	}
}

func TestChatStreamAggregatesReasoningAndToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat-1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"先想\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好的\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\"}}]}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	client, err := NewClient(Endpoint{BaseURL: server.URL, APIKey: "secret"}, "vl", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.StreamWithProtocol(context.Background(), ChatCompletions, Request{Input: []Item{{Type: "message", Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{response.Output[0].Type, response.Output[1].Type, response.Output[2].Type}; !reflect.DeepEqual(got, []string{"reasoning", "message", "tool_call"}) {
		t.Fatalf("output: %#v", response.Output)
	}
	if response.Output[2].CallID != "call-1" || response.Output[2].Arguments != "{}" {
		t.Fatalf("tool call: %#v", response.Output[2])
	}
}

func TestResponsesStreamDoesNotInventEmptyMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	client, err := NewClient(Endpoint{BaseURL: server.URL, APIKey: "secret"}, "qwen", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Stream(context.Background(), Request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 0 {
		t.Fatalf("invented output: %#v", response.Output)
	}
}
