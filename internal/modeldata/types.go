package modeldata

import (
	"encoding/json"

	"go-harness/harness/content"
)

const (
	ToolOutcomeUnknown = "TOOL_OUTCOME_UNKNOWN"
	ToolNotStarted     = "TOOL_NOT_STARTED"
)

type ToolRequest struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// AssistantOutputBlock preserves model output order. The v4 writer does not
// flatten text around tool calls.
type AssistantOutputBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type AssistantMessage struct {
	Content        string                 `json:"content"`
	MaxTokens      bool                   `json:"maxTokens,omitempty"`
	RawOutput      []content.ContentBlock `json:"rawOutput,omitempty"`
	OutputBlocks   []AssistantOutputBlock `json:"outputBlocks,omitempty"`
	ToolCalls      []ToolRequest          `json:"toolCalls,omitempty"`
	AttemptID      string                 `json:"attemptId,omitempty"`
	DurationMillis int64                  `json:"durationMillis,omitempty"`
	Usage          *ModelUsage            `json:"usage,omitempty"`
	// StreamRecords is the real provider attempt stream assembled by the native
	// loop. Nil means a non-streaming test model supplied this message.
	StreamRecords []any `json:"-"`
	StreamCount   int   `json:"-"`
	Interrupted   bool  `json:"-"`
}

// Provider-reported token telemetry, not Credits balance or a charge.
type ModelUsage struct {
	InputTokens              int64 `json:"inputTokens"`
	OutputTokens             int64 `json:"outputTokens"`
	TotalTokens              int64 `json:"totalTokens"`
	CachedInputTokens        int64 `json:"cachedInputTokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens,omitempty"`
	ReasoningTokens          int64 `json:"reasoningTokens,omitempty"`
}

type ToolResult struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Content       string                 `json:"content"`
	ContentBlocks []content.ContentBlock `json:"contentBlocks,omitempty"`
	IsError       bool                   `json:"isError,omitempty"`
	Code          string                 `json:"code,omitempty"`
	ErrorName     string                 `json:"errorName,omitempty"`
	Meta          json.RawMessage        `json:"meta,omitempty"`
	ConcludeTurn  bool                   `json:"concludeTurn,omitempty"`
}
