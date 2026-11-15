package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go-harness/harness/content"
	"go-harness/internal/modeldata"
)

// Message is the model-facing projection of the durable v4 session surface.
type Message struct {
	Role                   string                           `json:"role"`
	Content                string                           `json:"content"`
	ContentBlocks          []content.ContentBlock           `json:"contentBlocks,omitempty"`
	ContentBlocksComplete  bool                             `json:"contentBlocksComplete,omitempty"`
	OrderedAssistantBlocks []modeldata.AssistantOutputBlock `json:"orderedAssistantBlocks,omitempty"`
	ToolCalls              []modeldata.ToolRequest          `json:"toolCalls,omitempty"`
	ToolCallID             string                           `json:"toolCallId,omitempty"`
	ToolName               string                           `json:"toolName,omitempty"`
	Source                 *MessageSource                   `json:"source,omitempty"`
	SourceSeq              uint64                           `json:"-"`
	OffloadedImageIndexes  map[int]struct{}                 `json:"-"`
}

func (m Message) IsImageOffloaded(index int) bool {
	_, ok := m.OffloadedImageIndexes[index]
	return ok
}

type MessageSource struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	ThroughSeq uint64 `json:"throughSeq"`
	GoalID     string `json:"goalId,omitempty"`
	Revision   int    `json:"revision,omitempty"`
	Round      int    `json:"round,omitempty"`
}

type ToolSpec struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Parameters   json.RawMessage `json:"parameters"`
	ParallelSafe bool            `json:"-"`
	Timeout      time.Duration   `json:"-"`
}

type Model interface {
	Complete(context.Context, []Message, []ToolSpec) (modeldata.AssistantMessage, error)
}

// ModelStreamChunk is one DeepSeek-compatible block/usage/finish chunk from a
// live provider request. It is not yet a durable Session event.
type ModelStreamChunk map[string]any

type ModelStreamSink func(ModelStreamChunk) error

// StreamingModel is the formal Host model path. Complete remains available to
// test doubles and non-turn auxiliary calls while the native Agent loop moves
// to streaming attempts.
type StreamingModel interface {
	StreamChunks(context.Context, []Message, []ToolSpec, ModelStreamSink) (modeldata.AssistantMessage, error)
}

type Summarizer interface {
	Summarize(context.Context, []Message, []ToolSpec) (SummaryResult, error)
}

type SummaryResult struct {
	Text  string
	Usage *modeldata.ModelUsage
}

type Tool interface {
	Spec() ToolSpec
	Validate(json.RawMessage) error
	Execute(context.Context, json.RawMessage) (string, error)
}

type ContentTool interface {
	Tool
	ExecuteContent(context.Context, json.RawMessage) (string, []content.ContentBlock, error)
}

// PresentedTool projects a tool value into model text and replay metadata.
type PresentedTool interface {
	Tool
	ExecutePresented(context.Context, json.RawMessage) (string, json.RawMessage, error)
}

type Limits struct {
	MaxSteps              int
	MaxToolCalls          int
	MaxModelCalls         int
	MaxTurnDuration       time.Duration
	MaxParallelToolCalls  int
	RepeatReminder        *RepeatReminderConfig
	MaxContextBytes       int
	ContextWindowTokens   int `json:",omitempty"`
	ReservedOutputTokens  int `json:",omitempty"`
	ContextHeadroomTokens int `json:",omitempty"`
	MaxCompactionCalls    int
}

const defaultMaxContextBytes = 512 << 10
const maxSummaryBytes = 16 << 10
const MaxConfiguredContextTokens = 1 << 30

var ErrModelCallBudget = errors.New("turn model-call budget exhausted")
var ErrToolOutcomeUnknown = errors.New("tool outcome is unknown")
var ErrInvalidProviderUsage = errors.New("provider returned invalid token usage")

func (l Limits) ValidateContextWindow() error {
	if l.ContextWindowTokens < 0 || l.ReservedOutputTokens < 0 || l.ContextHeadroomTokens < 0 ||
		l.ContextWindowTokens > MaxConfiguredContextTokens || l.ReservedOutputTokens > MaxConfiguredContextTokens || l.ContextHeadroomTokens > MaxConfiguredContextTokens ||
		(l.ContextWindowTokens == 0 && (l.ReservedOutputTokens != 0 || l.ContextHeadroomTokens != 0)) ||
		(l.ContextWindowTokens > 0 && (l.ReservedOutputTokens == 0 || l.ReservedOutputTokens >= l.ContextWindowTokens || l.ContextHeadroomTokens >= l.ContextWindowTokens-l.ReservedOutputTokens)) {
		return fmt.Errorf("invalid model context window, output reservation, or headroom")
	}
	return nil
}

// ImageOffloadRequiredError asks the loop to omit additional old image
// occurrences from the next model request without changing durable content.
type ImageOffloadRequiredError struct{ Count int }

func (e *ImageOffloadRequiredError) Error() string {
	if e == nil {
		return "image offload required"
	}
	return fmt.Sprintf("image request exceeds route budget; offload %d occurrence(s)", e.Count)
}

type imageOffloadTarget struct {
	Seq          uint64 `json:"seq"`
	ImageIndexes []int  `json:"imageIndexes"`
}

type imageOffloadEvent struct {
	Targets []imageOffloadTarget `json:"targets"`
}

func waitModelRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func ModelInputDigest(messages []Message, specs []ToolSpec) (string, error) {
	encoded, err := json.Marshal(struct {
		Messages []Message  `json:"messages"`
		Tools    []ToolSpec `json:"tools"`
	}{Messages: messages, Tools: specs})
	if err != nil {
		return "", fmt.Errorf("encode model request envelope: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
