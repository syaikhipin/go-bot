package agent

import (
	"errors"
	"fmt"
	"strings"

	"go-harness/internal/modeldata"
	"go-harness/thirdparty/llm"
)

// llmStreamProjection translates OpenAI-compatible SSE events into the block
// vocabulary used by DeepSeek's AssistantStreamAttempt. It emits only deltas
// actually observed on the wire; a provider's completed item may close a
// block, but it never retroactively invents token deltas.
type llmStreamProjection struct {
	protocol llm.Protocol
	emit     ModelStreamSink
	blocks   map[llmStreamKey]*llmStreamBlock
	next     int
	finished bool
}

type llmStreamKey struct{ output, content int }

type llmStreamBlock struct {
	index  int
	kind   string
	closed bool
}

func newLLMStreamProjection(protocol llm.Protocol, emit ModelStreamSink) *llmStreamProjection {
	return &llmStreamProjection{protocol: protocol, emit: emit, blocks: make(map[llmStreamKey]*llmStreamBlock)}
}

func (p *llmStreamProjection) begin(key llmStreamKey, kind string) (*llmStreamBlock, error) {
	if block := p.blocks[key]; block != nil {
		if block.kind != kind {
			return nil, fmt.Errorf("model stream block %d:%d changed type", key.output, key.content)
		}
		return block, nil
	}
	block := &llmStreamBlock{index: p.next, kind: kind}
	if err := p.emit(ModelStreamChunk{"type": "block-start", "index": block.index, "blockType": kind}); err != nil {
		return nil, err
	}
	p.next++
	p.blocks[key] = block
	return block, nil
}

func (p *llmStreamProjection) delta(key llmStreamKey, kind, text string) error {
	if text == "" {
		return nil
	}
	block, err := p.begin(key, kind)
	if err != nil {
		return err
	}
	if block.closed {
		return errors.New("model stream sent a delta after block-end")
	}
	chunkType := "text-delta"
	if kind == "reasoning" {
		chunkType = "reasoning-delta"
	}
	return p.emit(ModelStreamChunk{"type": chunkType, "index": block.index, "text": text})
}

func (p *llmStreamProjection) toolDelta(key llmStreamKey, item *llm.Item, argumentDelta string) error {
	if item == nil || item.CallID == "" {
		return errors.New("model stream tool fragment has no call ID")
	}
	block, err := p.begin(key, "tool-call")
	if err != nil {
		return err
	}
	if block.closed {
		return errors.New("model stream sent tool arguments after block-end")
	}
	chunk := ModelStreamChunk{"type": "tool-call-delta", "index": block.index, "id": item.CallID, "argumentsDelta": argumentDelta}
	if item.Name != "" {
		chunk["name"] = item.Name
	}
	return p.emit(chunk)
}

func (p *llmStreamProjection) end(key llmStreamKey, kind string, content ModelStreamChunk) error {
	block, err := p.begin(key, kind)
	if err != nil || block.closed {
		return err
	}
	if err := p.emit(ModelStreamChunk{"type": "block-end", "index": block.index, "block": content}); err != nil {
		return err
	}
	block.closed = true
	return nil
}

func (p *llmStreamProjection) endItem(output int, item *llm.Item) error {
	if item == nil {
		return nil
	}
	switch item.Type {
	case "message", "reasoning":
		kind := "text"
		if item.Type == "reasoning" {
			kind = "reasoning"
		}
		for contentIndex, content := range item.Content {
			if content.Type != "text" {
				return fmt.Errorf("unsupported streamed output content %q", content.Type)
			}
			if err := p.end(llmStreamKey{output, contentIndex}, kind, ModelStreamChunk{"type": kind, "text": content.Text}); err != nil {
				return err
			}
		}
	case "tool_call":
		if item.CallID == "" || item.Name == "" {
			return errors.New("completed streamed tool call lacks identity")
		}
		if err := p.end(llmStreamKey{output, 0}, "tool-call", ModelStreamChunk{
			"type": "tool-call", "id": item.CallID, "name": item.Name, "arguments": item.Arguments,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (p *llmStreamProjection) endResponseItems(response *llm.Response) error {
	if p.protocol == llm.ChatCompletions {
		toolIndex := 2
		for _, item := range response.Output {
			output := 1
			switch item.Type {
			case "reasoning":
				output = 0
			case "tool_call":
				output = toolIndex
				toolIndex++
			}
			if err := p.endItem(output, &item); err != nil {
				return err
			}
		}
		return nil
	}
	for output, item := range response.Output {
		if err := p.endItem(output, &item); err != nil {
			return err
		}
	}
	return nil
}

func (p *llmStreamProjection) finish(response *llm.Response) error {
	if response == nil {
		return errors.New("model stream terminal event has no response")
	}
	if err := p.endResponseItems(response); err != nil {
		return err
	}
	if response.Usage != nil {
		usage, err := nativeUsage(&modeldata.ModelUsage{
			InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens,
			TotalTokens: response.Usage.TotalTokens, CachedInputTokens: response.Usage.CachedTokens,
			ReasoningTokens: response.Usage.ReasoningTokens,
		})
		if err != nil {
			return err
		}
		if err := p.emit(ModelStreamChunk{"type": "usage", "usage": usage}); err != nil {
			return err
		}
	}
	reason := "stop"
	switch {
	case (response.Status == "completed" || response.Status == "") && len(p.blocks) == 0:
		if err := p.emit(ModelStreamChunk{"type": "finish", "reason": ModelStreamChunk{
			"kind": "error", "failure": ModelStreamChunk{"code": "EMPTY_RESPONSE", "message": "model returned a completed response with no content"},
		}}); err != nil {
			return err
		}
		p.finished = true
		return nil
	case response.Status == "incomplete" && response.FinishReason == "max_output_tokens", response.Status == "length" && response.FinishReason == "length":
		reason = "max-tokens"
	case response.Status == "completed" || response.Status == "":
		for _, item := range response.Output {
			if item.Type == "tool_call" {
				reason = "tool-calls"
				break
			}
		}
	default:
		if err := p.emit(ModelStreamChunk{"type": "finish", "reason": ModelStreamChunk{
			"kind": "error", "failure": ModelStreamChunk{"code": "UPSTREAM", "message": "model stream ended with status " + response.Status},
		}}); err != nil {
			return err
		}
		p.finished = true
		return nil
	}
	if err := p.emit(ModelStreamChunk{"type": "finish", "reason": ModelStreamChunk{"kind": reason}}); err != nil {
		return err
	}
	p.finished = true
	return nil
}

func (p *llmStreamProjection) push(event llm.StreamEvent) error {
	if p.finished {
		return errors.New("model stream emitted data after finish")
	}
	key := llmStreamKey{event.OutputIndex, event.ContentIndex}
	switch event.Type {
	case "response.output_item.added":
		if event.Item != nil && event.Item.Type == "tool_call" {
			_, err := p.begin(llmStreamKey{event.OutputIndex, 0}, "tool-call")
			return err
		}
	case "response.output_text.delta", "message.delta":
		return p.delta(key, "text", event.Delta)
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "reasoning.delta":
		return p.delta(key, "reasoning", event.Delta)
	case "response.function_call_arguments.delta", "tool_call.arguments.delta":
		if event.Delta == "" {
			if event.Item == nil || event.Item.Type != "tool_call" {
				return nil
			}
			_, err := p.begin(llmStreamKey{event.OutputIndex, 0}, "tool-call")
			return err
		}
		return p.toolDelta(llmStreamKey{event.OutputIndex, 0}, event.Item, event.Delta)
	case "response.output_item.done":
		return p.endItem(event.OutputIndex, event.Item)
	case "response.completed", "response.incomplete", "response.failed", "response.cancelled", "completed":
		return p.finish(event.Response)
	case "response.function_call_arguments.done":
		// The completed item closes the block; arguments.done supplies only a
		// snapshot and is not itself another token delta.
	default:
		if strings.HasPrefix(event.Type, "response.") {
			return nil
		}
		return fmt.Errorf("unsupported model stream event %q", event.Type)
	}
	return nil
}
