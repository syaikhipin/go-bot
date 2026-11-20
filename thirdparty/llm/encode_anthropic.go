package llm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

type anthropicMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

// encodeAnthropic keeps tool results attached to their call, and groups parallel
// results before any queued user text in the following user message.
func encodeAnthropic(model string, request Request, stream bool) ([]byte, error) {
	var system []any
	messages := []anthropicMessage{}
	pending := map[string]bool{}
	add := func(role string, blocks []any, result bool) error {
		if len(blocks) == 0 {
			return nil
		}
		if role != "user" && role != "assistant" {
			return fmt.Errorf("unsupported Anthropic role %q", role)
		}
		if role == "assistant" && len(pending) > 0 && (len(messages) == 0 || messages[len(messages)-1].Role != "assistant") {
			return fmt.Errorf("Anthropic tool calls are missing results")
		}
		if len(messages) == 0 || messages[len(messages)-1].Role != role {
			messages = append(messages, anthropicMessage{Role: role, Content: []any{}})
		}
		last := &messages[len(messages)-1]
		if result {
			// Results must precede text, including steering admitted between tools.
			pos := 0
			for pos < len(last.Content) {
				b, ok := last.Content[pos].(map[string]any)
				if !ok || b["type"] != "tool_result" {
					break
				}
				pos++
			}
			next := append([]any{}, last.Content[:pos]...)
			next = append(next, blocks...)
			last.Content = append(next, last.Content[pos:]...)
		} else {
			last.Content = append(last.Content, blocks...)
		}
		return nil
	}
	for _, item := range request.Input {
		switch item.Type {
		case "message":
			parts, err := anthropicParts(item.Content)
			if err != nil {
				return nil, err
			}
			if item.Role == "system" || item.Role == "developer" {
				system = append(system, parts...)
				continue
			}
			if err := add(item.Role, parts, false); err != nil {
				return nil, err
			}
		case "reasoning":
			// Portable reasoning text has no Anthropic signature. Only native messages
			// from the same endpoint/model may replay verified thinking blocks.
		case "anthropic_message":
			if len(pending) > 0 {
				return nil, fmt.Errorf("Anthropic native message before pending tool results")
			}
			var blocks []json.RawMessage
			if err := json.Unmarshal(item.Raw, &blocks); err != nil {
				return nil, fmt.Errorf("invalid native Anthropic content: %w", err)
			}
			parts := make([]any, 0, len(blocks))
			nextPending := map[string]bool{}
			for _, raw := range blocks {
				var b struct {
					Type string `json:"type"`
					ID   string `json:"id"`
				}
				if err := json.Unmarshal(raw, &b); err != nil {
					return nil, err
				}
				if b.Type == "tool_use" {
					if b.ID == "" || nextPending[b.ID] {
						return nil, fmt.Errorf("invalid native tool call ID")
					}
					nextPending[b.ID] = true
				}
				parts = append(parts, json.RawMessage(raw))
			}
			if err := add("assistant", parts, false); err != nil {
				return nil, err
			}
			pending = nextPending
		case "tool_call":
			var input map[string]json.RawMessage
			if item.CallID == "" || item.Name == "" || json.Unmarshal([]byte(item.Arguments), &input) != nil || input == nil {
				return nil, fmt.Errorf("invalid Anthropic tool call")
			}
			if pending[item.CallID] {
				return nil, fmt.Errorf("duplicate Anthropic tool call ID")
			}
			if err := add("assistant", []any{map[string]any{"type": "tool_use", "id": item.CallID, "name": item.Name, "input": input}}, false); err != nil {
				return nil, err
			}
			pending[item.CallID] = true
		case "tool_result":
			if !pending[item.CallID] {
				return nil, fmt.Errorf("Anthropic tool result has no pending call %q", item.CallID)
			}
			parts, err := anthropicParts(item.Content)
			if err != nil {
				return nil, err
			}
			if err := add("user", []any{map[string]any{"type": "tool_result", "tool_use_id": item.CallID, "content": parts, "is_error": item.IsError}}, true); err != nil {
				return nil, err
			}
			delete(pending, item.CallID)
		default:
			return nil, fmt.Errorf("unsupported Anthropic input item %q", item.Type)
		}
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("Anthropic tool calls are missing results")
	}
	maxTokens := request.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = 16 * 1024
	}
	payload := map[string]any{"model": model, "messages": messages, "max_tokens": maxTokens, "stream": stream}
	if len(system) > 0 {
		payload["system"] = system
	}
	if len(request.Tools) > 0 {
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, map[string]any{"name": tool.Name, "description": tool.Description, "input_schema": toolParameters(tool)})
		}
		payload["tools"] = tools
		choice := defaultToolChoice(request.ToolChoice)
		if choice == "required" {
			choice = "any"
		}
		if choice != "auto" && choice != "any" && choice != "none" {
			return nil, fmt.Errorf("unsupported Anthropic tool choice %q", choice)
		}
		payload["tool_choice"] = map[string]string{"type": choice}
	}
	return json.Marshal(payload)
}

func anthropicParts(blocks []ContentBlock) ([]any, error) {
	parts := []any{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, map[string]string{"type": "text", "text": b.Text})
			}
		case "image":
			if b.Source == nil || b.Source.Type != "url" || b.Source.URL == "" {
				return nil, fmt.Errorf("image must be resolved before Anthropic request")
			}
			source := map[string]string{"type": "url", "url": b.Source.URL}
			if strings.HasPrefix(b.Source.URL, "data:") {
				header, data, ok := strings.Cut(strings.TrimPrefix(b.Source.URL, "data:"), ",")
				mime, encoded := strings.CutSuffix(header, ";base64")
				if !ok || !encoded {
					return nil, fmt.Errorf("invalid Anthropic image data URL")
				}
				switch mime {
				case "image/jpeg", "image/png", "image/gif", "image/webp":
				default:
					return nil, fmt.Errorf("unsupported Anthropic image type %q", mime)
				}
				if _, err := base64.StdEncoding.DecodeString(data); err != nil {
					return nil, fmt.Errorf("invalid image base64: %w", err)
				}
				source = map[string]string{"type": "base64", "media_type": mime, "data": data}
			}
			parts = append(parts, map[string]any{"type": "image", "source": source})
		default:
			return nil, fmt.Errorf("unsupported Anthropic content %q", b.Type)
		}
	}
	return parts, nil
}
