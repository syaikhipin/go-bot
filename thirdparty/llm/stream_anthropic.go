package llm

import (
	"encoding/json"
	"fmt"
	"io"
)

type anthropicStreamBlock struct {
	raw        map[string]any
	args       string
	fragmented bool
	closed     bool
}

func (b *anthropicStreamBlock) item() (Item, error) {
	if b.raw["type"] == "tool_use" && b.fragmented {
		return Item{Type: "tool_call", CallID: stringField(b.raw, "id"), Name: stringField(b.raw, "name"), Arguments: b.args}, nil
	}
	data, err := json.Marshal(b.raw)
	if err != nil {
		return Item{}, err
	}
	return anthropicItem(data, true)
}
func stringField(m map[string]any, key string) string { s, _ := m[key].(string); return s }

func consumeAnthropicStream(body io.Reader, sink StreamSink, activity func()) (*Response, error) {
	result := &Response{}
	blocks := []*anthropicStreamBlock{}
	var usage anthropicUsage
	var started, deltaReceived, finished bool
	var reason string
	err := readSSE(body, func(data []byte) (bool, error) {
		var event struct {
			Type    string         `json:"type"`
			Index   int            `json:"index"`
			Block   map[string]any `json:"content_block"`
			Message struct {
				ID    string          `json:"id"`
				Usage json.RawMessage `json:"usage"`
			} `json:"message"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				Thinking   string `json:"thinking"`
				Signature  string `json:"signature"`
				Partial    string `json:"partial_json"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			return false, fmt.Errorf("decode Anthropic stream: %w", err)
		}
		switch event.Type {
		case "ping":
			return false, nil
		case "error":
			return false, decodeAPIError(0, data)
		case "message_start":
			if started {
				return false, fmt.Errorf("duplicate Anthropic message_start")
			}
			started = true
			result.ID = event.Message.ID
			if err := mergeAnthropicUsage(&usage, event.Message.Usage); err != nil {
				return false, err
			}
		case "content_block_start":
			if !started || deltaReceived || event.Index != len(blocks) || event.Block == nil {
				return false, fmt.Errorf("invalid Anthropic block start")
			}
			block := &anthropicStreamBlock{raw: event.Block}
			blocks = append(blocks, block)
			item, err := block.item()
			if err != nil {
				return false, err
			}
			if err := sink(StreamEvent{Type: "anthropic.block.start", OutputIndex: event.Index, Item: &item}); err != nil {
				return false, err
			}
		case "content_block_delta":
			if event.Index < 0 || event.Index >= len(blocks) || blocks[event.Index].closed || deltaReceived {
				return false, fmt.Errorf("Anthropic delta outside open block")
			}
			block := blocks[event.Index]
			emitted := StreamEvent{OutputIndex: event.Index}
			switch event.Delta.Type {
			case "text_delta":
				if block.raw["type"] != "text" {
					return false, fmt.Errorf("Anthropic text delta changed block type")
				}
				block.raw["text"] = stringField(block.raw, "text") + event.Delta.Text
				emitted.Type = "message.delta"
				emitted.Delta = event.Delta.Text
			case "thinking_delta":
				if block.raw["type"] != "thinking" {
					return false, fmt.Errorf("Anthropic thinking delta changed block type")
				}
				block.raw["thinking"] = stringField(block.raw, "thinking") + event.Delta.Thinking
				emitted.Type = "reasoning.delta"
				emitted.Delta = event.Delta.Thinking
			case "signature_delta":
				if block.raw["type"] != "thinking" {
					return false, fmt.Errorf("Anthropic signature outside thinking")
				}
				block.raw["signature"] = stringField(block.raw, "signature") + event.Delta.Signature
				return false, nil
			case "input_json_delta":
				if block.raw["type"] != "tool_use" {
					return false, fmt.Errorf("Anthropic input delta outside tool")
				}
				block.fragmented = true
				block.args += event.Delta.Partial
				emitted.Type = "tool_call.arguments.delta"
				emitted.Delta = event.Delta.Partial
			default:
				return false, nil // Protocol permits new delta types.
			}
			item, err := block.item()
			if err != nil {
				return false, err
			}
			emitted.Item = &item
			return false, sink(emitted)
		case "content_block_stop":
			if event.Index < 0 || event.Index >= len(blocks) || blocks[event.Index].closed || deltaReceived {
				return false, fmt.Errorf("invalid Anthropic block stop")
			}
			block := blocks[event.Index]
			block.closed = true
			item, err := block.item()
			if err != nil {
				return false, err
			}
			return false, sink(StreamEvent{Type: "anthropic.block.stop", OutputIndex: event.Index, Item: &item})
		case "message_delta":
			if !started {
				return false, fmt.Errorf("Anthropic message delta before start")
			}
			for _, block := range blocks {
				if !block.closed {
					return false, fmt.Errorf("Anthropic message ended with open block")
				}
			}
			deltaReceived = true
			if event.Delta.StopReason != "" {
				reason = event.Delta.StopReason
			}
			if err := mergeAnthropicUsage(&usage, event.Usage); err != nil {
				return false, err
			}
		case "message_stop":
			if !started || !deltaReceived || reason == "" {
				return false, fmt.Errorf("Anthropic stop without final delta")
			}
			for _, block := range blocks {
				if !block.closed {
					return false, fmt.Errorf("Anthropic stop with open block")
				}
				item, err := block.item()
				if err != nil {
					return false, err
				}
				if block.raw["type"] == "tool_use" {
					var object map[string]json.RawMessage
					if json.Unmarshal([]byte(item.Arguments), &object) != nil || object == nil {
						if reason != "max_tokens" {
							return false, fmt.Errorf("invalid Anthropic tool arguments JSON")
						}
					} else {
						block.raw["input"] = object
					}
				}
				result.Output = append(result.Output, item)
				raw, err := json.Marshal(block.raw)
				if err != nil {
					return false, err
				}
				result.NativeContent = append(result.NativeContent, raw)
			}
			if !usage.valid() {
				return false, fmt.Errorf("invalid Anthropic token usage")
			}
			result.Usage = usage.usage()
			finishErr := anthropicFinish(result, reason)
			if finishErr != nil && reason != "max_tokens" {
				return false, finishErr
			}
			if err := sink(StreamEvent{Type: "completed", Response: result}); err != nil {
				return false, err
			}
			finished = true
			return true, finishErr
		default:
			return false, nil
		}
		return false, nil
	}, activity)
	if !finished && len(result.Output) == 0 {
		for _, b := range blocks {
			if item, e := b.item(); e == nil {
				result.Output = append(result.Output, item)
			}
		}
	}
	if started && usage.valid() {
		result.Usage = usage.usage()
	}
	if err != nil {
		return result, err
	}
	if !finished {
		return result, fmt.Errorf("Anthropic stream ended without message_stop: %w", io.ErrUnexpectedEOF)
	}
	return result, nil
}

func mergeAnthropicUsage(dst *anthropicUsage, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]int64
	// usage may include nested cache details; only decode known counters.
	var parts map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return err
	}
	fields = map[string]int64{}
	for _, key := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		if value, ok := parts[key]; ok {
			var count int64
			if err := json.Unmarshal(value, &count); err != nil || count < 0 {
				return fmt.Errorf("invalid Anthropic usage %s", key)
			}
			fields[key] = count
		}
	}
	if v, ok := fields["input_tokens"]; ok {
		dst.Input = v
	}
	if v, ok := fields["output_tokens"]; ok {
		dst.Output = v
	}
	if v, ok := fields["cache_read_input_tokens"]; ok {
		dst.Read = v
	}
	if v, ok := fields["cache_creation_input_tokens"]; ok {
		dst.Write = v
	}
	return nil
}
