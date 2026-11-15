package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

func decodeResponses(data []byte) (*Response, error) {
	var wire struct {
		ID                string `json:"id"`
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []json.RawMessage `json:"output"`
		Usage  struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
			InputDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("decode Responses: %w", err)
	}
	if wire.Error != nil {
		return nil, &APIError{Code: wire.Error.Code, Message: wire.Error.Message}
	}
	result := &Response{ID: wire.ID, Status: wire.Status, Output: make([]Item, 0, len(wire.Output)), Usage: &Usage{InputTokens: wire.Usage.InputTokens, OutputTokens: wire.Usage.OutputTokens, TotalTokens: wire.Usage.TotalTokens, CachedTokens: wire.Usage.InputDetails.CachedTokens, ReasoningTokens: wire.Usage.OutputDetails.ReasoningTokens}}
	if wire.IncompleteDetails != nil {
		result.FinishReason = wire.IncompleteDetails.Reason
	}
	for _, raw := range wire.Output {
		var item struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Role      string          `json:"role"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Content   json.RawMessage `json:"content"`
			Summary   json.RawMessage `json:"summary"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("decode Responses output item: %w", err)
		}
		value := Item{ID: item.ID, Role: item.Role, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments, Raw: append(json.RawMessage(nil), raw...)}
		switch item.Type {
		case "message":
			value.Type = "message"
			value.Content = decodeTextParts(item.Content)
		case "reasoning":
			value.Type = "reasoning"
			value.Content = decodeTextParts(item.Content)
			if len(value.Content) == 0 {
				value.Content = decodeTextParts(item.Summary)
			}
		case "function_call":
			value.Type = "tool_call"
		default:
			value.Type = item.Type // retain unknown items in their original position
		}
		result.Output = append(result.Output, value)
	}
	if wire.Status != "" && wire.Status != "completed" {
		return result, fmt.Errorf("Responses ended with status %q", wire.Status)
	}
	return result, nil
}

func decodeTextParts(raw json.RawMessage) []ContentBlock {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]ContentBlock, 0, len(parts))
	for _, part := range parts {
		if part.Text != "" {
			out = append(out, ContentBlock{Type: "text", Text: part.Text})
		}
	}
	return out
}

func decodeChatCompletion(data []byte) (*Response, error) {
	var wire struct {
		ID      string `json:"id"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			PromptDetails    struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("decode Chat Completions: %w", err)
	}
	if wire.Error != nil {
		return nil, &APIError{Code: wire.Error.Code, Message: wire.Error.Message}
	}
	if len(wire.Choices) == 0 {
		return nil, fmt.Errorf("Chat Completions returned no choices")
	}
	choice := wire.Choices[0].Message
	output := make([]Item, 0, len(choice.ToolCalls)+2)
	if strings.TrimSpace(choice.ReasoningContent) != "" {
		output = append(output, Item{Type: "reasoning", Content: []ContentBlock{{Type: "text", Text: choice.ReasoningContent}}})
	}
	if choice.Content != "" {
		output = append(output, Item{Type: "message", Role: choice.Role, Content: []ContentBlock{{Type: "text", Text: choice.Content}}})
	}
	for _, call := range choice.ToolCalls {
		output = append(output, Item{Type: "tool_call", CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	result := &Response{ID: wire.ID, Status: "completed", FinishReason: wire.Choices[0].FinishReason, Output: output, Usage: &Usage{InputTokens: wire.Usage.PromptTokens, OutputTokens: wire.Usage.CompletionTokens, TotalTokens: wire.Usage.TotalTokens, CachedTokens: wire.Usage.PromptDetails.CachedTokens, ReasoningTokens: wire.Usage.CompletionDetails.ReasoningTokens}}
	if reason := wire.Choices[0].FinishReason; reason == "length" || reason == "content_filter" {
		result.Status = reason
		return result, fmt.Errorf("Chat Completions ended with finish_reason %q", reason)
	}
	return result, nil
}
