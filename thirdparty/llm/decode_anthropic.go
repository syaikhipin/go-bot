package llm

import (
	"encoding/json"
	"fmt"
	"math"
)

type anthropicUsage struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
	Read   int64 `json:"cache_read_input_tokens"`
	Write  int64 `json:"cache_creation_input_tokens"`
}

func (u anthropicUsage) valid() bool {
	return u.Input >= 0 && u.Output >= 0 && u.Read >= 0 && u.Write >= 0 && u.Read <= math.MaxInt64-u.Write && u.Input <= math.MaxInt64-u.Read-u.Write && u.Output <= math.MaxInt64-u.Input-u.Read-u.Write
}

func (u anthropicUsage) usage() *Usage {
	input := u.Input + u.Read + u.Write
	return &Usage{InputTokens: input, OutputTokens: u.Output, TotalTokens: input + u.Output, CachedTokens: u.Read, CacheCreationTokens: u.Write}
}

func anthropicItem(raw json.RawMessage, partial bool) (Item, error) {
	var b struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return Item{}, err
	}
	switch b.Type {
	case "text":
		return Item{Type: "message", Role: "assistant", Content: []ContentBlock{{Type: "text", Text: b.Text}}}, nil
	case "thinking":
		return Item{Type: "reasoning", Content: []ContentBlock{{Type: "text", Text: b.Thinking}}}, nil
	case "redacted_thinking":
		return Item{Type: "reasoning"}, nil
	case "tool_use":
		var object map[string]json.RawMessage
		if b.ID == "" || b.Name == "" || !partial && (json.Unmarshal(b.Input, &object) != nil || object == nil) {
			return Item{}, fmt.Errorf("invalid Anthropic tool use")
		}
		return Item{Type: "tool_call", CallID: b.ID, Name: b.Name, Arguments: string(b.Input)}, nil
	default:
		return Item{}, fmt.Errorf("unsupported Anthropic output block %q", b.Type)
	}
}
func anthropicFinish(result *Response, reason string) error {
	result.FinishReason = reason
	switch reason {
	case "end_turn", "stop_sequence", "tool_use":
		result.Status = "completed"
		return nil
	case "max_tokens":
		result.Status = "incomplete"
		result.FinishReason = "max_output_tokens"
		return fmt.Errorf("Anthropic response reached max_tokens")
	case "refusal":
		result.Status = "refusal"
		return &APIError{Code: "content_policy", Message: "Anthropic refused this request"}
	case "model_context_window_exceeded":
		result.Status = "incomplete"
		return &APIError{Code: "context_length_exceeded", Message: "Anthropic model context length exceeded"}
	default:
		result.Status = "incomplete"
		return fmt.Errorf("unsupported Anthropic stop reason %q", reason)
	}
}
func decodeAnthropic(data []byte) (*Response, error) {
	var wire struct {
		ID         string            `json:"id"`
		Type       string            `json:"type"`
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      anthropicUsage    `json:"usage"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("decode Anthropic message: %w", err)
	}
	if wire.Type == "error" {
		return nil, decodeAPIError(0, data)
	}
	if !wire.Usage.valid() {
		return nil, fmt.Errorf("invalid Anthropic token usage")
	}
	result := &Response{ID: wire.ID, Usage: wire.Usage.usage(), NativeContent: wire.Content}
	for _, raw := range wire.Content {
		item, err := anthropicItem(raw, wire.StopReason == "max_tokens")
		if err != nil {
			return result, err
		}
		result.Output = append(result.Output, item)
	}
	return result, anthropicFinish(result, wire.StopReason)
}
