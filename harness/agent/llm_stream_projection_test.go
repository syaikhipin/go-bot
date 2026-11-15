package agent

import (
	"reflect"
	"testing"

	"go-harness/thirdparty/llm"
)

func TestLLMStreamProjectionKeepsRealInterleavedDeltasAndSettlement(t *testing.T) {
	var chunks []ModelStreamChunk
	p := newLLMStreamProjection(llm.Responses, func(chunk ModelStreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	events := []llm.StreamEvent{
		{Type: "response.output_item.added", OutputIndex: 0, Item: &llm.Item{Type: "message"}},
		{Type: "response.output_text.delta", OutputIndex: 0, Delta: "hel"},
		{Type: "response.output_item.added", OutputIndex: 1, Item: &llm.Item{Type: "tool_call", CallID: "call-1", Name: "readfile"}},
		{Type: "response.function_call_arguments.delta", OutputIndex: 1, Delta: "{\"p\":", Item: &llm.Item{Type: "tool_call", CallID: "call-1", Name: "readfile", Arguments: "{\"p\":"}},
		{Type: "response.output_text.delta", OutputIndex: 0, Delta: "lo"},
		{Type: "response.function_call_arguments.delta", OutputIndex: 1, Delta: "1}", Item: &llm.Item{Type: "tool_call", CallID: "call-1", Name: "readfile", Arguments: "{\"p\":1}"}},
		{Type: "response.output_item.done", OutputIndex: 0, Item: &llm.Item{Type: "message", Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}},
		{Type: "response.output_item.done", OutputIndex: 1, Item: &llm.Item{Type: "tool_call", CallID: "call-1", Name: "readfile", Arguments: "{\"p\":1}"}},
		{Type: "response.completed", Response: &llm.Response{Status: "completed", Output: []llm.Item{
			{Type: "message", Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}},
			{Type: "tool_call", CallID: "call-1", Name: "readfile", Arguments: "{\"p\":1}"},
		}, Usage: &llm.Usage{InputTokens: 9, OutputTokens: 5, TotalTokens: 14}}},
	}
	for _, event := range events {
		if err := p.push(event); err != nil {
			t.Fatal(err)
		}
	}
	types := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		types = append(types, chunk["type"].(string))
	}
	want := []string{"block-start", "text-delta", "block-start", "tool-call-delta", "text-delta", "tool-call-delta", "block-end", "block-end", "usage", "finish"}
	if !reflect.DeepEqual(types, want) || chunks[1]["text"] != "hel" || chunks[4]["text"] != "lo" || chunks[9]["reason"].(ModelStreamChunk)["kind"] != "tool-calls" {
		t.Fatalf("projected chunks = %#v", chunks)
	}
	if chunks[3]["argumentsDelta"] != "{\"p\":" || chunks[5]["argumentsDelta"] != "1}" {
		t.Fatalf("tool start fabricated an empty argument delta: %#v", chunks)
	}
}

func TestLLMStreamProjectionDoesNotInventTextDeltasFromFinalItem(t *testing.T) {
	var chunks []ModelStreamChunk
	p := newLLMStreamProjection(llm.ChatCompletions, func(chunk ModelStreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	response := &llm.Response{Status: "completed", Output: []llm.Item{{Type: "message", Content: []llm.ContentBlock{{Type: "text", Text: "whole"}}}}}
	if err := p.push(llm.StreamEvent{Type: "completed", Response: response}); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 || chunks[0]["type"] != "block-start" || chunks[1]["type"] != "block-end" || chunks[2]["type"] != "finish" {
		t.Fatalf("final-only response fabricated deltas: %#v", chunks)
	}
}

func TestLLMStreamProjectionChatToolStartDoesNotInventEmptyArguments(t *testing.T) {
	var chunks []ModelStreamChunk
	p := newLLMStreamProjection(llm.ChatCompletions, func(chunk ModelStreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	call := &llm.Item{Type: "tool_call", CallID: "call-1", Name: "echo"}
	if err := p.push(llm.StreamEvent{Type: "tool_call.arguments.delta", OutputIndex: 2, Item: call}); err != nil {
		t.Fatal(err)
	}
	call.Arguments = "{}"
	if err := p.push(llm.StreamEvent{Type: "tool_call.arguments.delta", OutputIndex: 2, Item: call, Delta: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := p.push(llm.StreamEvent{Type: "completed", Response: &llm.Response{Status: "completed", Output: []llm.Item{*call}}}); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 4 || chunks[0]["type"] != "block-start" || chunks[1]["type"] != "tool-call-delta" ||
		chunks[1]["argumentsDelta"] != "{}" || chunks[2]["type"] != "block-end" || chunks[3]["type"] != "finish" {
		t.Fatalf("Chat tool start emitted a phantom argument delta: %#v", chunks)
	}
}

func TestLLMStreamProjectionMarksCompletedEmptyResponseAsFailedAttempt(t *testing.T) {
	var chunks []ModelStreamChunk
	p := newLLMStreamProjection(llm.Responses, func(chunk ModelStreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err := p.push(llm.StreamEvent{Type: "response.completed", Response: &llm.Response{Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0]["type"] != "finish" ||
		chunks[0]["reason"].(ModelStreamChunk)["kind"] != "error" ||
		chunks[0]["reason"].(ModelStreamChunk)["failure"].(ModelStreamChunk)["code"] != "EMPTY_RESPONSE" {
		t.Fatalf("empty response finished as success: %#v", chunks)
	}
}
