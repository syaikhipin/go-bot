package llm

import (
	"encoding/json"
	"fmt"
	"io"
)

func consumeChatStream(body io.Reader, sink StreamSink, activity func()) (*Response, error) {
	result := &Response{Status: "completed"}
	reasoning := ""
	message := ""
	role := "assistant"
	toolCalls := make(map[int]*Item)
	done := false
	err := readSSE(body, func(data []byte) (bool, error) {
		if string(data) == "[DONE]" {
			done = true
			return true, nil
		}
		var chunk struct {
			ID      string `json:"id"`
			Choices []struct {
				Index        int    `json:"index"`
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Role             string `json:"role"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
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
		if err := json.Unmarshal(data, &chunk); err != nil {
			return false, fmt.Errorf("decode Chat Completions stream chunk: %w", err)
		}
		if chunk.Error != nil {
			return false, &APIError{Code: chunk.Error.Code, Message: chunk.Error.Message}
		}
		if chunk.ID != "" {
			result.ID = chunk.ID
		}
		if chunk.Usage != nil {
			result.Usage = &Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens, TotalTokens: chunk.Usage.TotalTokens, CachedTokens: chunk.Usage.PromptDetails.CachedTokens, ReasoningTokens: chunk.Usage.CompletionDetails.ReasoningTokens}
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if choice.FinishReason != "" {
				result.FinishReason = choice.FinishReason
			}
			delta := choice.Delta
			if delta.Role != "" {
				role = delta.Role
			}
			if delta.ReasoningContent != "" {
				reasoning += delta.ReasoningContent
				if err := sink(StreamEvent{Type: "reasoning.delta", OutputIndex: 0, Delta: delta.ReasoningContent}); err != nil {
					return false, err
				}
			}
			if delta.Content != "" {
				message += delta.Content
				if err := sink(StreamEvent{Type: "message.delta", OutputIndex: 1, Delta: delta.Content}); err != nil {
					return false, err
				}
			}
			for _, fragment := range delta.ToolCalls {
				call := toolCalls[fragment.Index]
				if call == nil {
					call = &Item{Type: "tool_call"}
					toolCalls[fragment.Index] = call
				}
				if fragment.ID != "" {
					call.CallID = fragment.ID
				}
				if fragment.Function.Name != "" {
					call.Name = fragment.Function.Name
				}
				call.Arguments += fragment.Function.Arguments
				if fragment.ID != "" || fragment.Function.Name != "" || fragment.Function.Arguments != "" {
					if err := sink(StreamEvent{Type: "tool_call.arguments.delta", OutputIndex: fragment.Index + 2, Delta: fragment.Function.Arguments, Item: cloneItem(call)}); err != nil {
						return false, err
					}
				}
			}
		}
		return false, nil
	}, activity)
	if reasoning != "" {
		result.Output = append(result.Output, Item{Type: "reasoning", Content: []ContentBlock{{Type: "text", Text: reasoning}}})
	}
	if message != "" {
		result.Output = append(result.Output, Item{Type: "message", Role: role, Content: []ContentBlock{{Type: "text", Text: message}}})
	}
	for _, item := range orderedItems(toolCalls) {
		result.Output = append(result.Output, item)
	}
	if err != nil {
		result.Status = ""
		if len(result.Output) > 0 || result.Usage != nil {
			return result, err
		}
		return nil, err
	}
	if !done {
		result.Status = ""
		return result, fmt.Errorf("Chat Completions stream ended without DONE")
	}
	if reason := result.FinishReason; reason == "length" || reason == "content_filter" {
		result.Status = reason
	}
	if err := sink(StreamEvent{Type: "completed", Response: result}); err != nil {
		return result, err
	}
	if reason := result.FinishReason; reason == "length" || reason == "content_filter" {
		return result, fmt.Errorf("Chat Completions ended with finish_reason %q", reason)
	}
	return result, nil
}
