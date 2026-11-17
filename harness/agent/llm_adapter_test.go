package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go-bot/internal/modeldata"
	"go-bot/internal/testsupport/session"
	"go-bot/thirdparty/llm"
)

type capturingLLMTransport struct {
	protocol llm.Protocol
	request  llm.Request
	response *llm.Response
	err      error
	calls    int
}

type recordingImageReader struct {
	data  []byte
	loads int
	path  string
}

func (r *recordingImageReader) LoadImage(_ context.Context, _ session.ImageAttachment) ([]byte, error) {
	r.loads++
	return r.data, nil
}

func (r *recordingImageReader) ImageAccessPath(_ session.ImageAttachment) string { return r.path }

func (c *capturingLLMTransport) GenerateWithProtocol(_ context.Context, protocol llm.Protocol, request llm.Request) (*llm.Response, error) {
	c.calls++
	c.protocol, c.request = protocol, request
	return c.response, c.err
}

func TestLLMAdapterStreamDeliversProviderDeltaBeforeCompletion(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected model path %q", r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client, err := llm.NewClient(llm.Endpoint{BaseURL: server.URL, APIKey: "test-key"}, "test-model", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deltas := make(chan llm.StreamEvent, 1)
	result := make(chan struct {
		message modeldata.AssistantMessage
		err     error
	}, 1)
	go func() {
		message, err := (LLMAdapter{Transport: client, Protocol: llm.ChatCompletions}).Stream(ctx,
			[]Message{{Role: "user", Content: "say hello"}}, nil, func(event llm.StreamEvent) error {
				if event.Type == "message.delta" {
					deltas <- event
				}
				return nil
			})
		result <- struct {
			message modeldata.AssistantMessage
			err     error
		}{message, err}
	}()
	select {
	case event := <-deltas:
		if event.Delta != "hello" {
			t.Fatalf("streamed delta = %q", event.Delta)
		}
	case <-ctx.Done():
		t.Fatal("adapter did not publish the provider delta before completion")
	}
	select {
	case <-result:
		t.Fatal("adapter returned before the provider completed")
	default:
	}
	close(release)
	select {
	case got := <-result:
		if got.err != nil || got.message.Content != "hello" {
			t.Fatalf("stream result = %+v, %v", got.message, got.err)
		}
	case <-ctx.Done():
		t.Fatal("adapter did not complete after the provider finished")
	}
}

func TestLLMAdapterStreamChunksKeepsProviderFinishAndToolArguments(t *testing.T) {
	for _, test := range []struct {
		name, payload string
		protocol      llm.Protocol
	}{
		{"chat", strings.Join([]string{
			`data: {"choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"answer","tool_calls":[{"index":0,"id":"call-1","function":{"name":"readfile","arguments":"{\"p\":"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
			`data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
			"data: [DONE]",
		}, "\n\n") + "\n\n", llm.ChatCompletions},
		{"responses", strings.Join([]string{
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`,
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"answer"}`,
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}}`,
			`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call-1","name":"readfile","arguments":""}}`,
			`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"p\":1}"}`,
			`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call-1","name":"readfile","arguments":"{\"p\":1}"}}`,
			`data: {"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":4,"output_tokens":2}}}`,
		}, "\n\n") + "\n\n", llm.Responses},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(test.payload))
			}))
			defer server.Close()
			client, err := llm.NewClient(llm.Endpoint{BaseURL: server.URL, APIKey: "test-key"}, "model", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			var chunks []ModelStreamChunk
			answer, err := (LLMAdapter{Transport: client, Protocol: test.protocol}).StreamChunks(context.Background(),
				[]Message{{Role: "user", Content: "go"}}, nil, func(chunk ModelStreamChunk) error {
					chunks = append(chunks, chunk)
					return nil
				})
			if err != nil || answer.Content != "answer" || len(answer.ToolCalls) != 1 || answer.ToolCalls[0].Name != "readfile" || answer.ToolCalls[0].Arguments != `{"p":1}` ||
				len(chunks) < 6 || chunks[len(chunks)-1]["type"] != "finish" || chunks[len(chunks)-1]["reason"].(ModelStreamChunk)["kind"] != "tool-calls" {
				t.Fatalf("%s streamed tool result = %+v, chunks=%#v, err=%v", test.name, answer, chunks, err)
			}
			var argumentDeltas string
			for _, chunk := range chunks {
				if chunk["type"] == "tool-call-delta" {
					argumentDeltas += chunk["argumentsDelta"].(string)
				}
			}
			if argumentDeltas != `{"p":1}` {
				t.Fatalf("%s tool argument deltas = %q", test.name, argumentDeltas)
			}
		})
	}
}

func TestLLMAdapterAcceptsMaxTokenPartialTextButDropsToolCalls(t *testing.T) {
	for _, test := range []struct {
		name, status, reason string
	}{
		{"chat", "length", "length"},
		{"responses", "incomplete", "max_output_tokens"},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &capturingLLMTransport{response: &llm.Response{
				Status: test.status, FinishReason: test.reason,
				Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5},
				Output: []llm.Item{
					{Type: "message", Content: []llm.ContentBlock{{Type: "text", Text: "partial"}}},
					{Type: "tool_call", CallID: "truncated", Name: "echo", Arguments: `{}`},
				},
			}, err: errors.New("provider output was truncated")}
			response, err := (LLMAdapter{Transport: transport}).Complete(context.Background(), []Message{{Role: "user", Content: "go"}}, nil)
			if err != nil || !response.MaxTokens || response.Content != "partial" || len(response.ToolCalls) != 0 || len(response.OutputBlocks) != 1 || response.OutputBlocks[0].Text != "partial" || response.Usage == nil || response.Usage.OutputTokens != 5 {
				t.Fatalf("max-token partial response was lost or unsafe: response=%+v err=%v", response, err)
			}
		})
	}
	transport := &capturingLLMTransport{response: &llm.Response{Status: "incomplete", FinishReason: "content_filter",
		Output: []llm.Item{{Type: "message", Content: []llm.ContentBlock{{Type: "text", Text: "filtered"}}}}}, err: errors.New("filtered")}
	if _, err := (LLMAdapter{Transport: transport}).Complete(context.Background(), []Message{{Role: "user", Content: "go"}}, nil); err == nil {
		t.Fatal("non-token incomplete response was accepted")
	}
}

func TestLLMAdapterMaxTokensThroughRealHTTPDecoder(t *testing.T) {
	for _, test := range []struct {
		name, path, payload string
		protocol            llm.Protocol
	}{
		{"chat", "/chat/completions", `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"partial","tool_calls":[{"id":"cut","function":{"name":"echo","arguments":"{}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, llm.ChatCompletions},
		{"responses", "/responses", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]},{"type":"function_call","call_id":"cut","name":"echo","arguments":"{}"}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`, llm.Responses},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.path {
					t.Errorf("request path %q, want %q", r.URL.Path, test.path)
				}
				_, _ = w.Write([]byte(test.payload))
			}))
			defer server.Close()
			client, err := llm.NewClient(llm.Endpoint{BaseURL: server.URL, APIKey: "test-key"}, "model", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			response, err := (LLMAdapter{Transport: client, Protocol: test.protocol}).Complete(context.Background(), []Message{{Role: "user", Content: "go"}}, nil)
			if err != nil || !response.MaxTokens || response.Content != "partial" || len(response.ToolCalls) != 0 || response.Usage == nil || response.Usage.TotalTokens != 15 {
				t.Fatalf("HTTP max-token finish was not retained safely: response=%+v err=%v", response, err)
			}
		})
	}
}

func TestLLMAdapterPreservesReasoningOnlyHTTPCompletion(t *testing.T) {
	for _, test := range []struct {
		name, path, payload string
		protocol            llm.Protocol
	}{
		{"chat", "/chat/completions", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":null,"reasoning_content":"thinking only"}}]}`, llm.ChatCompletions},
		{"responses", "/responses", `{"status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking only"}]}]}`, llm.Responses},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.path {
					t.Errorf("request path %q, want %q", r.URL.Path, test.path)
				}
				_, _ = w.Write([]byte(test.payload))
			}))
			defer server.Close()
			client, err := llm.NewClient(llm.Endpoint{BaseURL: server.URL, APIKey: "test-key"}, "model", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			response, err := (LLMAdapter{Transport: client, Protocol: test.protocol}).Complete(context.Background(), []Message{{Role: "user", Content: "think"}}, nil)
			if err != nil || response.Content != "" || response.MaxTokens || len(response.ToolCalls) != 0 ||
				len(response.OutputBlocks) != 1 || response.OutputBlocks[0].Type != "reasoning" || response.OutputBlocks[0].Text != "thinking only" {
				t.Fatalf("reasoning-only HTTP completion = %+v, %v", response, err)
			}
		})
	}
}

func TestLLMAdapterResolvesServerAgentImagesInContentOrder(t *testing.T) {
	transport := &capturingLLMTransport{response: &llm.Response{Output: []llm.Item{
		{Type: "message", Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "done"}}},
		{Type: "tool_call", CallID: "next", Name: "echo", Arguments: `{}`},
	}}}
	reader := &recordingImageReader{data: []byte("pixels")}
	ref := session.ImageAttachment{ID: "sha256:image", MIMEType: "image/png", Bytes: 6, Width: 2, Height: 3}
	adapter := LLMAdapter{Transport: transport, Images: reader, ImageInputEnabled: true}
	response, err := adapter.Complete(context.Background(), []Message{{Role: "user", ContentBlocks: []session.ContentBlock{
		{Type: "text", Text: "before"}, session.ImageBlock(ref), {Type: "text", Text: "after"},
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocks := transport.request.Input[0].Content
	if transport.protocol != llm.Responses || len(blocks) != 3 || blocks[0].Text != "before" || blocks[1].Type != "image" || blocks[2].Text != "after" || blocks[1].Source == nil || blocks[1].Source.URL != "data:image/png;base64,cGl4ZWxz" || reader.loads != 1 {
		t.Fatalf("image order or resolution lost: %#v, loads=%d", transport.request, reader.loads)
	}
	if response.Content != "done" || len(response.ToolCalls) != 1 || response.ToolCalls[0].ID != "next" {
		t.Fatalf("model output lost: %#v", response)
	}
}

func TestLLMAdapterKeepsToolResultThenResolvesImageInUserItem(t *testing.T) {
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed"}}
	reader := &recordingImageReader{data: []byte("pixels")}
	ref := session.ImageAttachment{ID: "sha256:image", MIMEType: "image/png", Bytes: 6, Width: 2, Height: 3}
	adapter := LLMAdapter{Transport: transport, Images: reader, ImageInputEnabled: true}
	_, err := adapter.Complete(context.Background(), []Message{
		{Role: "assistant", ToolCalls: []session.ToolRequest{{ID: "call-1", Name: "readimage", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "call-1", ToolName: "readimage", Content: "image ready", ContentBlocks: []session.ContentBlock{session.ImageBlock(ref), session.ImageBlock(ref)}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	input := transport.request.Input
	if len(input) != 4 || input[1].Type != "tool_call" || input[2].Type != "tool_result" || input[2].CallID != "call-1" || len(input[2].Content) != 1 || input[2].Content[0].Text != "image ready" || input[3].Type != "message" || input[3].Role != "user" || len(input[3].Content) != 3 || input[3].Content[1].Source == nil || input[3].Content[2].Source == nil || reader.loads != 1 {
		t.Fatalf("tool image projection is incorrect: %#v", input)
	}
}

func TestLLMAdapterExposesImageAttachmentIDToImageGeneration(t *testing.T) {
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed"}}
	adapter := LLMAdapter{Transport: transport, Images: &recordingImageReader{data: []byte("pixels")}, ImageInputEnabled: true}
	ref := session.ImageAttachment{ID: "sha256:user-image", MIMEType: "image/png", Bytes: 6, Width: 2, Height: 3}
	_, err := adapter.Complete(t.Context(), []Message{{Role: "user", ContentBlocks: []session.ContentBlock{session.ImageBlock(ref)}}},
		[]ToolSpec{{Name: "generateimage"}})
	if err != nil {
		t.Fatal(err)
	}
	blocks := transport.request.Input[0].Content
	if len(blocks) != 2 || blocks[0].Text != "Image attachment ID: "+ref.ID || blocks[1].Source == nil {
		t.Fatalf("could not identify the image in the model request: %#v", blocks)
	}
}

func TestLLMAdapterOffloadsOccurrenceBeforeTransportCall(t *testing.T) {
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed"}}
	reader := &recordingImageReader{data: []byte("pixels"), path: "/root/harness/.dsh/attachments/v1/objects/ab/immutable"}
	ref := session.ImageAttachment{ID: "sha256:image", MIMEType: "image/png", Bytes: 6, Width: 2, Height: 3, ReadPath: "a.png"}
	adapter := LLMAdapter{Transport: transport, Images: reader, ImageAccessPath: func(session.ImageAttachment) string { return reader.path }, ImageInputEnabled: true, MaxImages: 1, ImageCountQuantum: 1}
	message := Message{Role: "user", ContentBlocks: []session.ContentBlock{session.ImageBlock(ref), session.ImageBlock(ref)}}
	_, err := adapter.Complete(context.Background(), []Message{message}, nil)
	var offload *ImageOffloadRequiredError
	if !errors.As(err, &offload) || offload.Count != 1 || transport.calls != 0 || reader.loads != 1 {
		t.Fatalf("over-budget request escaped: %v, calls=%d loads=%d", err, transport.calls, reader.loads)
	}
	message.OffloadedImageIndexes = map[int]struct{}{0: {}}
	_, err = adapter.Complete(context.Background(), []Message{message}, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocks := transport.request.Input[0].Content
	if len(blocks) != 2 || !strings.Contains(blocks[0].Text, reader.path) || strings.Contains(blocks[0].Text, "a.png") || blocks[1].Source == nil {
		t.Fatalf("offloaded occurrence was not projected in place: %#v", blocks)
	}
}

func TestLLMAdapterOffloadedImageWithoutToolWorldPathDoesNotInventSourcePath(t *testing.T) {
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed"}}
	adapter := LLMAdapter{Transport: transport, Images: &recordingImageReader{}}
	ref := session.ImageAttachment{ID: "sha256:image", MIMEType: "image/png", Bytes: 6, Width: 2, Height: 3, ReadPath: "obsolete.png"}
	message := Message{Role: "user", ContentBlocks: []session.ContentBlock{session.ImageBlock(ref)}, OffloadedImageIndexes: map[int]struct{}{0: {}}}
	if _, err := adapter.Complete(context.Background(), []Message{message}, nil); err != nil {
		t.Fatal(err)
	}
	got := transport.request.Input[0].Content[0].Text
	if !strings.Contains(got, "No local normalized image path is available") || strings.Contains(got, "obsolete.png") {
		t.Fatalf("offloaded image invented a re-read path: %q", got)
	}
}

func TestLLMAdapterInlineImageBudgetMatchesPreparedBase64Bytes(t *testing.T) {
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed"}}
	reader := &recordingImageReader{data: []byte("abc")}
	ref := session.ImageAttachment{ID: "sha256:image", MIMEType: "image/png", Bytes: 3, Width: 2, Height: 2}
	adapter := LLMAdapter{Transport: transport, Images: reader, ImageInputEnabled: true,
		MaxInlineImageBytes: 4, InlineByteQuantum: 1}
	_, err := adapter.Complete(context.Background(), []Message{{Role: "user", ContentBlocks: []session.ContentBlock{session.ImageBlock(ref)}}}, nil)
	if err != nil || transport.calls != 1 {
		t.Fatalf("exact base64 budget was rejected: %v, calls=%d", err, transport.calls)
	}
	if got := transport.request.Input[0].Content[0].Source.URL; got != "data:image/png;base64,YWJj" {
		t.Fatalf("prepared image request = %q", got)
	}
}

func TestLLMAdapterRequestsQuantumOfOldestImageOccurrences(t *testing.T) {
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed"}}
	reader := &recordingImageReader{data: []byte("abc")}
	ref := session.ImageAttachment{ID: "sha256:image", MIMEType: "image/png", Bytes: 3, Width: 2, Height: 2}
	adapter := LLMAdapter{Transport: transport, Images: reader, ImageInputEnabled: true,
		MaxImages: 2, ImageCountQuantum: 2}
	message := Message{Role: "user", ContentBlocks: []session.ContentBlock{session.ImageBlock(ref), session.ImageBlock(ref), session.ImageBlock(ref)}}
	_, err := adapter.Complete(context.Background(), []Message{message}, nil)
	var offload *ImageOffloadRequiredError
	if !errors.As(err, &offload) || offload.Count != 2 || transport.calls != 0 {
		t.Fatalf("three images did not require oldest quantum: %v, calls=%d", err, transport.calls)
	}
	message.OffloadedImageIndexes = map[int]struct{}{0: {}, 1: {}}
	if _, err := adapter.Complete(context.Background(), []Message{message}, nil); err != nil {
		t.Fatal(err)
	}
	if transport.calls != 1 || len(transport.request.Input[0].Content) != 3 || transport.request.Input[0].Content[2].Source == nil {
		t.Fatalf("request after oldest quantum offload = %+v", transport.request)
	}
}

func TestLLMAdapterClassifiesEndpointError(t *testing.T) {
	transport := &capturingLLMTransport{err: &llm.APIError{StatusCode: 429, Code: "rate_limit", Message: "too many requests"}}
	_, err := (LLMAdapter{Transport: transport}).Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	var modelErr *llm.ModelError
	if !errors.As(err, &modelErr) || modelErr.Code != llm.ModelErrorUpstreamRateLimited || transport.calls != 1 {
		t.Fatalf("endpoint error was not classified: %v", err)
	}
}

func TestLLMAdapterCarriesAuxiliaryCallOptionsWithoutChangingNormalCalls(t *testing.T) {
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed"}}
	adapter := LLMAdapter{Transport: transport}
	ctx := WithModelCallOptions(context.Background(), ModelCallOptions{Purpose: "compaction", MaxOutputTokens: 2048})
	if _, err := adapter.Complete(ctx, []Message{{Role: "user", Content: "summarize"}}, nil); err != nil {
		t.Fatal(err)
	}
	if transport.request.Purpose != "compaction" || transport.request.MaxOutputTokens != 2048 {
		t.Fatalf("auxiliary model options missing: %+v", transport.request)
	}
	if _, err := adapter.Complete(context.Background(), []Message{{Role: "user", Content: "answer"}}, nil); err != nil {
		t.Fatal(err)
	}
	if transport.request.Purpose != "" || transport.request.MaxOutputTokens != 0 {
		t.Fatalf("auxiliary options leaked into normal call: %+v", transport.request)
	}
}

func TestLLMAdapterCarriesChildReasoningEffortWithoutLeakingToParent(t *testing.T) {
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed"}}
	adapter := LLMAdapter{Transport: transport}
	ctx := WithModelRoute(context.Background(), ModelRoute{Provider: "other", Model: "fast", ReasoningEffort: "high"})
	if _, err := adapter.Complete(ctx, []Message{{Role: "user", Content: "child"}}, nil); err != nil {
		t.Fatal(err)
	}
	if transport.request.ReasoningEffort != "high" {
		t.Fatalf("child effort missing from transport: %+v", transport.request)
	}
	if _, err := adapter.Complete(context.Background(), []Message{{Role: "user", Content: "parent"}}, nil); err != nil {
		t.Fatal(err)
	}
	if transport.request.ReasoningEffort != "" {
		t.Fatalf("child effort leaked to parent transport: %+v", transport.request)
	}
}

func TestLLMAdapterPreservesCompactionTextBlocks(t *testing.T) {
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed", Output: []llm.Item{
		{Type: "message", Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "first"}, {Type: "text", Text: " second"}}},
	}}}
	adapter := LLMAdapter{Transport: transport}
	ctx := WithModelCallOptions(context.Background(), ModelCallOptions{Purpose: "compaction"})
	response, err := adapter.Complete(ctx, []Message{{Role: "user", Content: "summarize"}}, nil)
	if err != nil || response.Content != "first second" || len(response.RawOutput) != 2 ||
		response.RawOutput[0].Text != "first" || response.RawOutput[1].Text != " second" {
		t.Fatalf("compaction output blocks were flattened: %#v, %v", response, err)
	}
	response, err = adapter.Complete(context.Background(), []Message{{Role: "user", Content: "answer"}}, nil)
	if err != nil || len(response.RawOutput) != 0 {
		t.Fatalf("auxiliary raw output leaked into ordinary call: %#v, %v", response, err)
	}
}

func TestLLMAdapterPreservesOrderedOrdinaryOutputAndProjectedInput(t *testing.T) {
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed", Output: []llm.Item{
		{Type: "reasoning", Content: []llm.ContentBlock{{Type: "text", Text: "private analysis"}}},
		{Type: "message", Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "before"}, {Type: "text", Text: " middle"}}},
		{Type: "tool_call", CallID: "call-1", Name: "echo", Arguments: `{}`},
		{Type: "message", Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: " after"}}},
	}}}
	message := Message{Role: "user", Content: "before\nafter", ContentBlocksComplete: true,
		ContentBlocks: []session.ContentBlock{{Type: "text", Text: "before"}, {Type: "text", Text: "after"}}}
	response, err := (LLMAdapter{Transport: transport}).Complete(context.Background(), []Message{message}, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocks := transport.request.Input[0].Content
	if len(blocks) != 2 || blocks[0].Text != "before" || blocks[1].Text != "after" {
		t.Fatalf("request blocks were duplicated or flattened: %+v", blocks)
	}
	if response.Content != "before middle after" || len(response.ToolCalls) != 1 || len(response.OutputBlocks) != 5 ||
		response.OutputBlocks[0].Type != "reasoning" || response.OutputBlocks[0].Text != "private analysis" ||
		response.OutputBlocks[1].Text != "before" || response.OutputBlocks[2].Text != " middle" ||
		response.OutputBlocks[3].Type != "tool-call" || response.OutputBlocks[3].ID != "call-1" ||
		response.OutputBlocks[4].Text != " after" {
		t.Fatalf("ordinary provider block order was lost: %+v", response)
	}
	_, err = (LLMAdapter{Transport: transport}).Complete(context.Background(), []Message{{Role: "assistant",
		OrderedAssistantBlocks: response.OutputBlocks, Content: response.Content, ToolCalls: response.ToolCalls}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"reasoning", "message", "tool_call", "message"}
	if got := transport.request.Input; len(got) != len(want) || got[0].Type != want[0] || got[0].Content[0].Text != "private analysis" ||
		got[1].Type != want[1] || len(got[1].Content) != 2 || got[2].Type != want[2] || got[3].Type != want[3] {
		t.Fatalf("ordered assistant history was not sent to model: %+v", got)
	}
}

func TestLLMAdapterRejectsImageOutputInCompaction(t *testing.T) {
	transport := &capturingLLMTransport{response: &llm.Response{Status: "completed", Output: []llm.Item{
		{Type: "message", Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "partial"}, {Type: "image"}}},
	}}}
	ctx := WithModelCallOptions(context.Background(), ModelCallOptions{Purpose: "compaction"})
	response, err := (LLMAdapter{Transport: transport}).Complete(ctx, nil, nil)
	if err == nil || response.Content != "" || len(response.RawOutput) != 0 {
		t.Fatalf("non-text summary output was accepted: %#v, %v", response, err)
	}
}

func TestLLMAdapterKeepsPartialUsageWithoutAcceptingPartialOutput(t *testing.T) {
	transport := &capturingLLMTransport{
		response: &llm.Response{Status: "incomplete", Output: []llm.Item{{Type: "message", Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "untrusted partial text"}}}}, Usage: &llm.Usage{InputTokens: 89, OutputTokens: 7, TotalTokens: 96}},
		err:      errors.New("Responses ended with status incomplete"),
	}
	response, err := (LLMAdapter{Transport: transport}).Complete(context.Background(), []Message{{Role: "user", Content: "task"}}, nil)
	if err == nil || response.Content != "" || response.Usage == nil || response.Usage.TotalTokens != 96 {
		t.Fatalf("partial response leaked or lost usage: %#v, %v", response, err)
	}
	var modelErr *llm.ModelError
	if !errors.As(err, &modelErr) || !modelErr.OutputObserved || modelErr.Code != llm.ModelErrorResponseIncomplete {
		t.Fatalf("partial response was not classified as observed output: %#v", modelErr)
	}
}

func TestLLMAdapterStreamErrorAfterDeltaIsNotRetryable(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol llm.Protocol
		body     string
	}{
		{"responses", llm.Responses,
			"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial\"}\n\n" +
				"data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"too many requests\"}\n\n"},
		{"chat", llm.ChatCompletions,
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n" +
				"data: {\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"too many requests\"}}\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := llm.NewClient(llm.Endpoint{BaseURL: server.URL, APIKey: "secret"}, "model", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			var observed bool
			response, err := (LLMAdapter{Transport: client, Protocol: test.protocol}).StreamChunks(context.Background(), nil, nil,
				func(chunk ModelStreamChunk) error {
					observed = observed || chunk["type"] == "text-delta"
					return nil
				})
			var modelErr *llm.ModelError
			_, retryable := nativeModelFailure(err)
			if !observed || !errors.As(err, &modelErr) || !modelErr.OutputObserved || retryable ||
				modelErr.Code != llm.ModelErrorUpstreamRateLimited || response.Content != "" || len(response.ToolCalls) != 0 {
				t.Fatalf("failed streamed attempt = %+v, %+v, observed=%t", response, modelErr, observed)
			}
		})
	}
}
