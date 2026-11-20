// Package llm provides endpoint-based clients for OpenAI-compatible language
// model APIs and Anthropic Messages. It does not know about application sessions or local resources.
package llm

import "encoding/json"

type Protocol string

const (
	Responses         Protocol = "responses"
	ChatCompletions   Protocol = "chat_completions"
	AnthropicMessages Protocol = "anthropic_messages"
)

type Endpoint struct {
	BaseURL string
	APIKey  string
}

type ImageSource struct {
	Type string `json:"type"` // url; business image refs are resolved before calling this package
	URL  string `json:"url"`
}

type ContentBlock struct {
	Type   string       `json:"type"` // text or image
	Text   string       `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"`
	Detail string       `json:"detail,omitempty"`
	Title  string       `json:"title,omitempty"`
	Alt    string       `json:"alt,omitempty"`
}

// Item is one ordered model input or output item. Optional upstream fields
// remain optional here; an agent decides whether a tool call is executable.
type Item struct {
	IsError   bool            `json:"isError,omitempty"`
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   []ContentBlock  `json:"content,omitempty"`
	CallID    string          `json:"callId,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

func (i Item) MarshalJSON() ([]byte, error) {
	if i.Type == "tool_result" {
		content := i.Content
		if content == nil {
			content = []ContentBlock{}
		}
		return json.Marshal(struct {
			Type    string         `json:"type"`
			CallID  string         `json:"callId,omitempty"`
			Content []ContentBlock `json:"content"`
			IsError bool           `json:"isError,omitempty"`
		}{i.Type, i.CallID, content, i.IsError})
	}
	if i.Type != "message" && i.Type != "reasoning" && i.Type != "tool_call" && len(i.Raw) > 0 {
		return append([]byte(nil), i.Raw...), nil
	}
	type plain Item
	return json.Marshal(plain(i))
}

func (i *Item) UnmarshalJSON(data []byte) error {
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &kind); err != nil {
		return err
	}
	if kind.Type == "tool_result" {
		var value struct {
			Type    string          `json:"type"`
			CallID  string          `json:"callId"`
			Content json.RawMessage `json:"content"`
			IsError bool            `json:"isError"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		var content []ContentBlock
		if err := json.Unmarshal(value.Content, &content); err != nil {
			return err
		}
		*i = Item{IsError: value.IsError, Type: value.Type, CallID: value.CallID, Content: content, Raw: append(json.RawMessage(nil), data...)}
		return nil
	}
	if kind.Type != "message" && kind.Type != "reasoning" && kind.Type != "tool_call" {
		var value struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(data, &value)
		*i = Item{Type: kind.Type, ID: value.ID, Raw: append(json.RawMessage(nil), data...)}
		return nil
	}
	type plain Item
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*i = Item(value)
	i.Raw = append(json.RawMessage(nil), data...)
	return nil
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type Request struct {
	Input           []Item
	Tools           []Tool
	ToolChoice      string // defaults to auto when tools are supplied
	ReasoningEffort string
	// Purpose is Harness telemetry. It is not a provider wire parameter.
	Purpose         string
	MaxOutputTokens int
}

type Usage struct {
	InputTokens         int64 `json:"inputTokens"`
	OutputTokens        int64 `json:"outputTokens"`
	TotalTokens         int64 `json:"totalTokens"`
	CachedTokens        int64 `json:"cachedTokens,omitempty"`
	CacheCreationTokens int64 `json:"cacheCreationTokens,omitempty"`
	// A subset of OutputTokens, never added again to the request total.
	ReasoningTokens int64 `json:"reasoningTokens,omitempty"`
}

type Response struct {
	// NativeContent is provider replay data, never a public transcript block.
	NativeContent []json.RawMessage `json:"-"`
	ID            string            `json:"id,omitempty"`
	Status        string            `json:"status,omitempty"`
	FinishReason  string            `json:"finishReason,omitempty"`
	Output        []Item            `json:"output"`
	Usage         *Usage            `json:"usage,omitempty"`
}

type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "model API request failed"
}
