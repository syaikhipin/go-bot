package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-bot/harness/session"
	"go-bot/internal/testsupport/session"
	"go-bot/thirdparty/llm"
)

func TestNativePruneContentMatchesDeepSeekHeadMiddleTail(t *testing.T) {
	config := nativePruneConfig{threshold: 50, head: 4, tail: 3}
	blocks := []map[string]json.RawMessage{
		{"type": json.RawMessage(`"text"`), "text": json.RawMessage(`"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`)},
		{"type": json.RawMessage(`"reasoning"`), "text": json.RawMessage(`"private-rich-block"`)},
		{"type": json.RawMessage(`"text"`), "text": json.RawMessage(`"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"`)},
		{"type": json.RawMessage(`"tool-call"`), "name": json.RawMessage(`"nested"`)},
		{"type": json.RawMessage(`"text"`), "text": json.RawMessage(`"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`)},
	}
	result, changed, err := pruneNativeContent(blocks, config)
	if err != nil || !changed || len(result) != 4 {
		t.Fatalf("prune content = %v, %v, %v", result, changed, err)
	}
	var head, tail string
	if err := json.Unmarshal(result[0]["text"], &head); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(result[3]["text"], &tail); err != nil {
		t.Fatal(err)
	}
	if head != "AAAA"+nativePruneMarker || tail != "CCC" || string(result[1]["type"]) != `"reasoning"` || string(result[2]["type"]) != `"tool-call"` {
		t.Fatalf("pruned order/content = %q, %q, %s, %s", head, tail, result[1]["type"], result[2]["type"])
	}
	unicodeBlocks := []map[string]json.RawMessage{{"type": json.RawMessage(`"text"`)}}
	unicodeBlocks[0]["text"], _ = json.Marshal(strings.Repeat("😀", 60))
	unicodeResult, changed, err := pruneNativeContent(unicodeBlocks, config)
	if err != nil || !changed {
		t.Fatalf("Unicode prune = %v, %v", changed, err)
	}
	var unicodeText string
	if err := json.Unmarshal(unicodeResult[0]["text"], &unicodeText); err != nil {
		t.Fatal(err)
	}
	if unicodeText != strings.Repeat("😀", 4)+nativePruneMarker+strings.Repeat("😀", 3) {
		t.Fatalf("Unicode prune split a code point: %q", unicodeText)
	}
}

type nativeLongTool struct {
	text  string
	calls int
}

func (*nativeLongTool) Spec() ToolSpec {
	return ToolSpec{Name: "echo", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (*nativeLongTool) Validate(json.RawMessage) error { return nil }
func (t *nativeLongTool) Execute(context.Context, json.RawMessage) (string, error) {
	t.calls++
	return t.text, nil
}

func TestNativeToolPruneReplacesOnlySurfaceAndRestores(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-prune", CreatedAt: 1}
	j := &nativeTestJournal{}
	s, err := sessionformat.NewLiveSession(header, j)
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeTestModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", []Tool{&nativeLongTool{text: strings.Repeat("😀", 100)}}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "run tool"); err != nil {
		t.Fatal(err)
	}
	var original sessionformat.Event
	for _, event := range s.Events() {
		if event.Type == "tool/result" {
			original = event
			break
		}
	}
	if original.Type == "" {
		t.Fatal("no original tool result")
	}
	if _, err := loop.append("turn/start", map[string]any{"turn": 2}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.append("step/start", map[string]any{"turn": 2, "step": 1}, nil, nil); err != nil {
		t.Fatal(err)
	}
	pruned, err := loop.pruneNativeToolResults(context.Background(), nativePruneConfig{threshold: 50, head: 4, tail: 3})
	if err != nil || pruned != 1 {
		t.Fatalf("native prune = %d, %v", pruned, err)
	}
	events := s.Events()
	if len(events) < 2 || events[len(events)-2].Type != "compaction/prune" || events[len(events)-1].Type != "tool/result" {
		t.Fatalf("prune pair missing at tail: %+v", events[len(events)-2:])
	}
	var price struct {
		ShadowedRange struct{ Start, End uint64 } `json:"shadowedRange"`
		ShadowedSeqs  []uint64                    `json:"shadowedSeqs"`
		Tokens        int                         `json:"shadowedTokenCount"`
	}
	if err := json.Unmarshal(events[len(events)-2].Data, &price); err != nil || price.ShadowedRange.Start != original.Seq || price.ShadowedRange.End != original.Seq || len(price.ShadowedSeqs) != 1 || price.ShadowedSeqs[0] != original.Seq || price.Tokens <= 0 {
		t.Fatalf("wrong prune provenance/price: %+v, %v", price, err)
	}
	var replacementData struct {
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(events[len(events)-1].Data, &replacementData); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events[original.Seq].Data), strings.Repeat("😀", 100)) || len(replacementData.Message.Content) != 1 || replacementData.Message.Content[0].Text != strings.Repeat("😀", 4)+nativePruneMarker+strings.Repeat("😀", 3) {
		t.Fatal("native prune mutated history or lost replacement marker")
	}
	if err := sessionformat.ValidateV4Relationships(events); err != nil {
		t.Fatal(err)
	}
	folded, err := sessionformat.FoldSurface(events)
	if err != nil || len(folded.Replacements) != 1 || folded.Replacements[0].ShadowedSeqs[0] != original.Seq {
		t.Fatalf("native prune did not replay: %+v, %v", folded.Replacements, err)
	}
	var surfacePruned bool
	for _, message := range folded.Messages {
		var parsed struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(message, &parsed); err != nil {
			t.Fatal(err)
		}
		for _, block := range parsed.Content {
			surfacePruned = surfacePruned || strings.Contains(block.Text, nativePruneMarker)
		}
	}
	if !surfacePruned {
		t.Fatal("model surface did not select the pruned result")
	}
	if _, err := loop.append("step/end", map[string]any{"turn": 2, "step": 1}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.append("turn/end", map[string]any{"turn": 2, "reason": map[string]any{"kind": "completed"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	again, err := loop.pruneNativeToolResults(context.Background(), nativePruneConfig{threshold: 50, head: 4, tail: 3})
	if err != nil || again != 0 {
		t.Fatalf("already-pruned result repeated: %d, %v", again, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

type nativeOverflowModel struct {
	calls    int
	observed []int
}

type nativeLongImageTool struct{}

func (nativeLongImageTool) Spec() ToolSpec {
	return ToolSpec{Name: "echo", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (nativeLongImageTool) Validate(json.RawMessage) error { return nil }
func (nativeLongImageTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "", nil
}
func (nativeLongImageTool) ExecuteContent(context.Context, json.RawMessage) (string, []session.ContentBlock, error) {
	return strings.Repeat("x", 100), []session.ContentBlock{{
		Type: "image", ImageRefID: "sha256:abc", MIMEType: "image/png", Bytes: 100, Width: 10, Height: 10,
	}}, nil
}

func TestNativePrunePreservesOffloadedImageProjection(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-prune-image", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(s, &nativeTestModel{}, "provider", "model", "prompt", []Tool{nativeLongImageTool{}}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "run tool"); err != nil {
		t.Fatal(err)
	}
	var original uint64
	for _, event := range s.Events() {
		if event.Type == "tool/result" {
			original = event.Seq
		}
	}
	if _, err := loop.append("image/offload", imageOffloadEvent{Targets: []imageOffloadTarget{{Seq: original, ImageIndexes: []int{0}}}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.append("turn/start", map[string]any{"turn": 2}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.append("step/start", map[string]any{"turn": 2, "step": 1}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if count, err := loop.pruneNativeToolResults(context.Background(), nativePruneConfig{threshold: 50, head: 4, tail: 3}); err != nil || count != 1 {
		t.Fatalf("prune with image offload = %d, %v", count, err)
	}
	var replacement struct {
		Message struct {
			Content []struct {
				Type      string `json:"type"`
				Offloaded bool   `json:"offloaded"`
			} `json:"content"`
		} `json:"message"`
	}
	events := s.Events()
	if err := json.Unmarshal(events[len(events)-1].Data, &replacement); err != nil {
		t.Fatal(err)
	}
	if len(replacement.Message.Content) != 2 || replacement.Message.Content[1].Type != "image" || !replacement.Message.Content[1].Offloaded || strings.Contains(string(events[original].Data), `"offloaded":true`) {
		t.Fatal("prune did not preserve projected image offload independently of original history")
	}
	if _, err := loop.append("step/end", map[string]any{"turn": 2, "step": 1}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.append("turn/end", map[string]any{"turn": 2, "reason": map[string]any{"kind": "completed"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
}

func (m *nativeOverflowModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	m.calls++
	if m.calls == 1 {
		return session.AssistantMessage{ToolCalls: []session.ToolRequest{{ID: "call-1", Name: "echo", Arguments: `{}`}}}, nil
	}
	length := 0
	for _, message := range messages {
		if message.Role == "tool" {
			length = len([]rune(message.Content))
		}
	}
	m.observed = append(m.observed, length)
	if length > nativePruneThreshold {
		return session.AssistantMessage{}, &llm.ModelError{Code: llm.ModelErrorContextLimitExceeded, Cause: errors.New("maximum context length exceeded")}
	}
	return session.AssistantMessage{Content: "done"}, nil
}

func TestNativeLoopPrunesOnContextOverflowWithoutRepeatingTool(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-prune-overflow", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model, tool := &nativeOverflowModel{}, &nativeLongTool{text: strings.Repeat("x", nativePruneThreshold+1000)}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", []Tool{tool}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := loop.RunTextTurn(context.Background(), "run tool")
	if err != nil || answer != "done" || model.calls != 3 || tool.calls != 1 {
		t.Fatalf("overflow recovery = %q, %v; model=%d tool=%d", answer, err, model.calls, tool.calls)
	}
	if len(model.observed) != 2 || model.observed[0] <= nativePruneThreshold || model.observed[1] > nativePruneThreshold {
		t.Fatalf("model did not see replacement surface: %v", model.observed)
	}
	counts := map[string]int{}
	for _, event := range s.Events() {
		counts[event.Type]++
	}
	if counts["compaction/prune"] != 1 || counts["llm/retry"] != 0 || counts["tool/call"] != 1 || counts["assistant/attempt"] != 1 || counts["step/start"] != 2 {
		t.Fatalf("overflow replayed a tool or consumed provider retry: %+v", counts)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
}
