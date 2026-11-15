package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"go-harness/harness/session"
	"go-harness/internal/testsupport/session"
)

type nativePressureModel struct {
	normal, summary int
	failSummary     bool
	summaryOptions  []ModelCallOptions
	usage           *session.ModelUsage
	normalRequests  [][]Message
}

type nativeUsageAnchorModel struct{ usage session.ModelUsage }

func TestNativeImagePressureKeepsFixedHeuristicWithoutRoutePricing(t *testing.T) {
	// The current generic OpenAI-compatible route does not declare provider-
	// specific visual pricing. Like DeepSeek's priceSurface fallback, each image
	// occurrence retains its own fixed heuristic even after an offload mark.
	const attachment = `{"attachmentId":"sha256:shared","mediaType":"image/png","bytes":128}`
	first := json.RawMessage(`{"role":"user","content":[{"type":"image","attachment":` + attachment + `}]}`)
	second := json.RawMessage(`{"role":"user","content":[{"type":"image","attachment":` + attachment + `,"offloaded":true}]}`)
	surface := sessionformat.Surface{
		Nodes: []uint64{5, 6}, Messages: []json.RawMessage{first, second}, MessageSeqs: []uint64{5, 6},
	}
	prices, err := nativeSurfacePrices(surface, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prices) != 2 {
		t.Fatalf("image occurrence prices = %+v", prices)
	}
	for index, message := range surface.Messages {
		want, err := estimateNativeMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		if prices[index].heuristic != want || prices[index].request != want {
			t.Fatalf("image occurrence %d price = %+v, want fixed %d", index, prices[index], want)
		}
	}
	got, err := nativeRequestTokens(surface, nil, nil)
	if err != nil || got != prices[0].request+prices[1].request {
		t.Fatalf("route-without-pricing request = %d, %v; occurrences = %+v", got, err, prices)
	}
}

func (m nativeUsageAnchorModel) Complete(context.Context, []Message, []ToolSpec) (session.AssistantMessage, error) {
	return session.AssistantMessage{Content: "Measured answer.", Usage: &m.usage}, nil
}

type nativePressureSchemaTool struct{}

func (nativePressureSchemaTool) Spec() ToolSpec {
	return ToolSpec{Name: "large_schema", Description: strings.Repeat("d", 2100), Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}
}

func (nativePressureSchemaTool) Validate(json.RawMessage) error { return nil }

func (nativePressureSchemaTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "unused", nil
}

type rejectCompactionEndJournal struct{ nativeTestJournal }

func (j *rejectCompactionEndJournal) Append(ctx context.Context, events []sessionformat.Event) error {
	for _, event := range events {
		if event.Type == "compaction/end" {
			return errors.New("compaction end persistence failed")
		}
	}
	return j.nativeTestJournal.Append(ctx, events)
}

func (m *nativePressureModel) Complete(ctx context.Context, messages []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	if len(messages) > 0 && messages[len(messages)-1].Content == nativeCompactionInstruction {
		m.summary++
		m.summaryOptions = append(m.summaryOptions, ModelCallOptionsFromContext(ctx))
		if m.failSummary {
			return session.AssistantMessage{}, errors.New("summary provider unavailable")
		}
		return session.AssistantMessage{Content: "The earlier work established the important result."}, nil
	}
	m.normal++
	m.normalRequests = append(m.normalRequests, append([]Message(nil), messages...))
	return session.AssistantMessage{Content: strings.Repeat("evidence ", 110), Usage: m.usage}, nil
}

func TestNativeLoopPressureRetainsCurrentInstructionsForSameStepRequest(t *testing.T) {
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "pressure-instructions", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativePressureModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "Help the user.", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	guidance := NativeInstructionSnapshot{
		Identity: "project", Text: "<system-reminder>\nInstructions from: AGENTS.md\n\npressure guidance\n</system-reminder>",
		Changes: []NativeInstructionChange{{Action: "set", Scope: ".\x00AGENTS.md", Path: "AGENTS.md", Digest: "pressure"}},
		Content: map[string]string{".\x00AGENTS.md": "pressure guidance"},
	}
	loop.SetInstructionProvider(func(context.Context, []string) (NativeInstructionSnapshot, error) { return guidance, nil })
	if err := loop.SetContextPressure(700, 32, 64); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		if _, err := loop.RunTextTurn(context.Background(), "Continue research with the available facts."); err != nil {
			t.Fatalf("turn %d: %v", index+1, err)
		}
	}
	if model.summary == 0 {
		t.Fatal("test did not exercise pre-request compaction")
	}
	for index, request := range model.normalRequests {
		seen := false
		for _, message := range request {
			seen = seen || strings.Contains(message.Content, "pressure guidance")
		}
		if !seen {
			t.Fatalf("normal request %d lost current instructions", index)
		}
	}
}

func TestNativeLoopPressureSummaryFailureContinuesWithOriginalSurface(t *testing.T) {
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "pressure-summary-failure", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativePressureModel{failSummary: true}
	loop, err := NewNativeLoop(s, model, "provider", "model", "Help the user.", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.SetContextPressure(500, 32, 64); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		if _, err := loop.RunTextTurn(context.Background(), "Continue research with the available facts."); err != nil {
			t.Fatalf("primary turn %d failed after rejected summary: %v", index+1, err)
		}
	}
	if model.summary == 0 || model.normal != 3 {
		t.Fatalf("summary failure interrupted the primary request: normal=%d summary=%d", model.normal, model.summary)
	}
	counts := map[string]int{}
	for _, event := range s.DurableEvents() {
		counts[event.Type]++
	}
	if counts["compaction/start"] == 0 || counts["compaction/end"] != counts["compaction/start"] || counts["compaction/summary"] != 0 {
		t.Fatalf("failed auto summary changed or left open a compaction: %+v", counts)
	}
	if err := sessionformat.ValidateV4Relationships(s.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	surface, err := s.Surface()
	if err != nil {
		t.Fatal(err)
	}
	if len(surface.Replacements) != 0 {
		t.Fatal("failed proactive summary replaced the original surface")
	}
}

func TestNativeLoopPressureDoesNotHideFailedCompactionClose(t *testing.T) {
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "pressure-close-failure", CreatedAt: 1}, &rejectCompactionEndJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativePressureModel{failSummary: true}
	loop, err := NewNativeLoop(s, model, "provider", "model", "Help the user.", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.SetContextPressure(500, 32, 0); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		_, err := loop.RunTextTurn(context.Background(), "Continue research with the available facts.")
		if model.summary == 0 {
			if err != nil {
				t.Fatalf("seed turn %d: %v", index+1, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "compaction end persistence failed") {
			t.Fatalf("failed compaction journal close was hidden: %v", err)
		}
		return
	}
	t.Fatal("pressure never requested a summary")
}

func TestNativeLoopCompactsAtConfiguredPreRequestPressure(t *testing.T) {
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "pressure", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativePressureModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "Help the user.", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.SetContextPressure(500, 32, 64); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		if _, err := loop.RunTextTurn(context.Background(), "Continue research with the available facts."); err != nil {
			t.Fatalf("turn %d: %v", index+1, err)
		}
	}
	if model.summary == 0 || model.normal != 3 {
		t.Fatalf("pressure did not compact before the third primary request: normal=%d summary=%d", model.normal, model.summary)
	}
	if len(model.summaryOptions) == 0 || model.summaryOptions[0].Purpose != "compaction" || model.summaryOptions[0].MaxOutputTokens != 64 {
		t.Fatalf("summary call lacks its purpose or output cap: %+v", model.summaryOptions)
	}
	var summarySeq, lastAssistantSeq uint64
	for _, event := range s.DurableEvents() {
		if event.Type == "compaction/summary" {
			summarySeq = event.Seq
			var summary struct {
				MaxTokens int `json:"maxTokens"`
			}
			if err := json.Unmarshal(event.Data, &summary); err != nil || summary.MaxTokens != 64 {
				t.Fatalf("summary cap not recorded in v4 event: %+v, %v", summary, err)
			}
		}
		if event.Type == "assistant/message" {
			lastAssistantSeq = event.Seq
		}
	}
	if summarySeq == 0 || lastAssistantSeq <= summarySeq {
		t.Fatal("pressure checkpoint was not committed before the current model answer")
	}
	if _, err := sessionformat.RestoreLiveSession(sessionformat.PhysicalArtifact{Header: s.Header(), Events: s.DurableEvents()}, &nativeTestJournal{}); err != nil {
		t.Fatalf("pre-request pressure journal cannot be restored: %v", err)
	}
}

func TestNativeLoopPressureIncludesRequestToolSchemas(t *testing.T) {
	for _, withTool := range []bool{false, true} {
		name := "without-tools"
		var tools []Tool
		if withTool {
			name = "with-tools"
			tools = []Tool{nativePressureSchemaTool{}}
		}
		s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: name, CreatedAt: 1}, &nativeTestJournal{})
		if err != nil {
			t.Fatal(err)
		}
		model := &nativePressureModel{}
		loop, err := NewNativeLoop(s, model, "provider", "model", "Help the user.", tools, 2, 2)
		if err != nil {
			t.Fatal(err)
		}
		if err := loop.SetContextPressure(1000, 32, 64); err != nil {
			t.Fatal(err)
		}
		for index := 0; index < 2; index++ {
			if _, err := loop.RunTextTurn(context.Background(), "Continue with the earlier evidence."); err != nil {
				t.Fatalf("%s turn %d: %v", name, index+1, err)
			}
		}
		if withTool && model.summary == 0 {
			t.Fatal("large request tool schema did not contribute to pressure")
		}
		if !withTool && model.summary != 0 {
			t.Fatal("message-only request compacted below pressure threshold")
		}
		if err := sessionformat.ValidateV4Relationships(s.DurableEvents()); err != nil {
			t.Fatalf("%s pressure journal is invalid: %v", name, err)
		}
	}
}

func TestNativeLoopNoPressureWithoutKnownContextWindow(t *testing.T) {
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "no-pressure", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativePressureModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "Help the user.", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		if _, err := loop.RunTextTurn(context.Background(), "Continue research."); err != nil {
			t.Fatal(err)
		}
	}
	if model.summary != 0 || model.normal != 3 {
		t.Fatalf("unknown capacity triggered compaction: normal=%d summary=%d", model.normal, model.summary)
	}
}

func TestNativeMillionTokenWindowPressureThreshold(t *testing.T) {
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "million-pressure", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(s, &nativePressureModel{}, "provider", "model", "Help the user.", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.SetContextPressure(1_000_000, 16*1024, 8*1024); err != nil {
		t.Fatal(err)
	}
	threshold, retain := loop.nativePressureThreshold()
	if threshold != 800_000 || retain != (1_000_000-16*1024)*16/100 {
		t.Fatalf("million-token pressure threshold/retention = %d/%d", threshold, retain)
	}
}

func TestNativeRequestPricesActualFileHandleText(t *testing.T) {
	ref := NativeFileAttachmentRef{ID: "sha256:" + strings.Repeat("a", 64), Name: "report.txt", Bytes: 100}
	message, err := json.Marshal(map[string]any{
		"role": "user", "content": []any{map[string]any{
			"type": "file", "attachment": map[string]any{"attachmentId": ref.ID, "name": ref.Name, "bytes": ref.Bytes},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	surface := sessionformat.Surface{Nodes: []uint64{4, 5}, Messages: []json.RawMessage{message}, MessageSeqs: []uint64{5}}
	fixed, err := nativeSurfaceTokens(surface)
	if err != nil {
		t.Fatal(err)
	}
	path := "/root/harness/inputs/" + strings.Repeat("nested/", 400) + "report.txt"
	resolve := func(found NativeFileAttachmentRef) string {
		if found != ref {
			t.Fatalf("file price resolver got %+v", found)
		}
		return path
	}
	priced, err := nativeRequestTokens(surface, nil, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if priced <= fixed+500 {
		t.Fatalf("model-visible file handle was not priced: fixed=%d request=%d", fixed, priced)
	}
	noPath, err := nativeRequestTokens(surface, nil, nil)
	if err != nil || noPath <= fixed || noPath >= priced {
		t.Fatalf("missing-path handle price = %d, fixed=%d, actual-path=%d, err=%v", noPath, fixed, priced, err)
	}
}

func TestNativeRequestPressureUsesDurableProviderAnchorAndSurvivesResume(t *testing.T) {
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "usage-anchor", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := nativeUsageAnchorModel{usage: session.ModelUsage{InputTokens: 2000, OutputTokens: 20, TotalTokens: 2020, CachedInputTokens: 100}}
	loop, err := NewNativeLoop(s, model, "provider", "model", "Help the user.", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "A small request."); err != nil {
		t.Fatal(err)
	}
	assertAnchor := func(t *testing.T, current *NativeLoop, record *sessionformat.LiveSession) {
		t.Helper()
		surface, err := record.Surface()
		if err != nil {
			t.Fatal(err)
		}
		fixed, err := nativeRequestTokens(surface, nil, nil)
		if err != nil || fixed >= 2020 {
			t.Fatalf("fixture does not separate heuristic/provider prices: fixed=%d err=%v", fixed, err)
		}
		measured, err := current.nativeMeasuredRequestTokens(surface)
		if err != nil || measured != 2020 {
			t.Fatalf("provider anchor = %d, want 2020: %v", measured, err)
		}
		current.provider = "another-provider"
		mismatch, err := current.nativeMeasuredRequestTokens(surface)
		if err != nil || mismatch != fixed {
			t.Fatalf("changed request envelope reused stale sample: %d, fixed=%d, %v", mismatch, fixed, err)
		}
		current.provider = "provider"
	}
	assertAnchor(t, loop, s)
	var stream []struct {
		Type  string `json:"type"`
		Chunk struct {
			Type string `json:"type"`
		} `json:"chunk"`
	}
	for _, event := range s.DurableEvents() {
		if event.Type != "assistant/message" {
			continue
		}
		var value struct {
			Stream []struct {
				Type  string `json:"type"`
				Chunk struct {
					Type string `json:"type"`
				} `json:"chunk"`
			} `json:"stream"`
		}
		if err := json.Unmarshal(event.Data, &value); err != nil {
			t.Fatal(err)
		}
		stream = value.Stream
	}
	if len(stream) != 4 || stream[0].Chunk.Type != "block-start" || stream[1].Chunk.Type != "block-end" || stream[2].Chunk.Type != "usage" || stream[3].Chunk.Type != "finish" {
		t.Fatalf("non-streaming response did not produce a truthful v4 settlement: %+v", stream)
	}
	restored, err := sessionformat.RestoreLiveSession(sessionformat.PhysicalArtifact{Header: s.Header(), Events: s.DurableEvents()}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := NewNativeLoop(restored, model, "provider", "model", "Help the user.", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	assertAnchor(t, resumed, restored)
	if err := sessionformat.ValidateV4Relationships(s.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("SERVERAGENTS_V4_USAGE_ANCHOR_EXPORT_PATH"); path != "" {
		if err := os.WriteFile(path, nativeTestJournalJSONL(t, s.Header(), &nativeTestJournal{events: s.DurableEvents()}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeRequestPressureRejectsUnderpricedProviderAnchor(t *testing.T) {
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "usage-anchor-too-small", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(s, nativeUsageAnchorModel{usage: session.ModelUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}}, "provider", "model", strings.Repeat("prompt ", 100), nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "Do the task."); err != nil {
		t.Fatal(err)
	}
	surface, err := s.Surface()
	if err != nil {
		t.Fatal(err)
	}
	estimated, err := nativeRequestTokens(surface, nil, nil)
	if err != nil || estimated <= 2 {
		t.Fatalf("fixture does not exceed provider report: %d, %v", estimated, err)
	}
	measured, err := loop.nativeMeasuredRequestTokens(surface)
	if err != nil || measured != estimated {
		t.Fatalf("underpriced provider sample was trusted: measured=%d estimated=%d err=%v", measured, estimated, err)
	}
}

func TestNativeToolPriceUsesCanonicalRequestHeaderSchema(t *testing.T) {
	compact := []ToolSpec{{Name: "editfile", Description: "edit", Parameters: json.RawMessage(`{"type":"object","properties":{"count":{"type":"number","minimum":1}}}`)}}
	formatted := []ToolSpec{{Name: "editfile", Description: "edit", Parameters: json.RawMessage("{\n  \"properties\": {\"count\": {\"minimum\": 1.0, \"type\": \"number\"}},\n  \"type\": \"object\"\n}")}}
	first, err := nativeToolTokens(compact)
	if err != nil {
		t.Fatal(err)
	}
	second, err := nativeToolTokens(formatted)
	if err != nil || first != second {
		t.Fatalf("equivalent request schemas have different prices: %d/%d, %v", first, second, err)
	}
	for _, specs := range [][]ToolSpec{compact, formatted} {
		canonical, err := nativeCanonicalTools(specs)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(canonical)
		if err != nil || !json.Valid(encoded) {
			t.Fatalf("canonical request tools = %s, %v", encoded, err)
		}
	}
}

func TestNativeRequestPriceDoesNotHTMLEscapeStructuralJSON(t *testing.T) {
	description := strings.Repeat("<>&", 20)
	tools := []ToolSpec{{Name: "tool", Description: description, Parameters: json.RawMessage(`{"type":"object"}`)}}
	gotTools, err := nativeToolTokens(tools)
	if err != nil {
		t.Fatal(err)
	}
	toolWire := `[{"description":"` + description + `","name":"tool","parameters":{"type":"object"}}]`
	if want := (len(toolWire)+3)/4 + 4; gotTools != want {
		t.Fatalf("HTML characters changed tool schema price: got %d, want %d", gotTools, want)
	}

	name := strings.Repeat("<", 20)
	imageBlock := `{"type":"image","attachment":{"attachmentId":"sha256:abc","mediaType":"image/png","bytes":3,"width":1,"height":1,"name":"` + name + `"}}`
	imageMessage := json.RawMessage(`{"role":"user","content":[` + imageBlock + `]}`)
	gotImage, err := estimateNativeMessage(imageMessage)
	if err != nil {
		t.Fatal(err)
	}
	imageWire := `{"attachment":{"attachmentId":"sha256:abc","bytes":3,"height":1,"mediaType":"image/png","name":"` + name + `","width":1},"type":"image"}`
	if want := 4 + (len(imageWire)+3)/4 + 4; gotImage != want {
		t.Fatalf("HTML characters changed image structural price: got %d, want %d", gotImage, want)
	}

	ref := NativeFileAttachmentRef{ID: "sha256:abc", Name: name, Bytes: 3}
	fileMessage := json.RawMessage(`{"role":"user","content":[{"type":"file","attachment":{"attachmentId":"sha256:abc","name":"` + name + `","bytes":3}}]}`)
	surface := sessionformat.Surface{Nodes: []uint64{1}, Messages: []json.RawMessage{fileMessage}, MessageSeqs: []uint64{1}}
	path := "/root/harness/workspaces/<file>"
	gotFile, err := nativeRequestTokens(surface, nil, func(NativeFileAttachmentRef) string { return path })
	if err != nil {
		t.Fatal(err)
	}
	handle := nativeFileHandleText(ref, path)
	if want := 4 + (len(handle)+3)/4 + 4; gotFile != want {
		t.Fatalf("HTML characters changed projected file price: got %d, want %d", gotFile, want)
	}
}

func TestNativePriceJSONMatchesJavaScriptLineSeparatorEscapes(t *testing.T) {
	encoded, err := nativePriceJSON(map[string]string{"actual": "\u2028\u2029", "literal": `\u2028`})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"actual\":\"\u2028\u2029\",\"literal\":\"\\\\u2028\"}"
	if string(encoded) != want {
		t.Fatalf("request JSON = %q, want %q", encoded, want)
	}
}

func TestNativePressureMeterConsumesOnlyNewDurableEvents(t *testing.T) {
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "incremental-meter", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := nativeUsageAnchorModel{usage: session.ModelUsage{InputTokens: 2000, OutputTokens: 20, TotalTokens: 2020}}
	loop, err := NewNativeLoop(s, model, "provider", "model", "Help the user.", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	firstSurface, err := s.Surface()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.nativeMeasuredRequestTokens(firstSurface); err != nil {
		t.Fatal(err)
	}
	firstAnchor := loop.pressureMeter.anchor
	firstCursor := loop.pressureMeter.cursor
	if firstAnchor == nil || firstCursor != len(s.DurableEvents()) {
		t.Fatalf("first meter cursor=%d anchor=%v", firstCursor, firstAnchor)
	}
	if _, err := loop.nativeMeasuredRequestTokens(firstSurface); err != nil {
		t.Fatal(err)
	}
	if loop.pressureMeter.anchor != firstAnchor || loop.pressureMeter.cursor != firstCursor {
		t.Fatal("unchanged durable log rebuilt the provider anchor")
	}
	if _, err := loop.RunTextTurn(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	secondSurface, err := s.Surface()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.nativeMeasuredRequestTokens(secondSurface); err != nil {
		t.Fatal(err)
	}
	if loop.pressureMeter.anchor == firstAnchor || loop.pressureMeter.cursor <= firstCursor || loop.pressureMeter.cursor != len(s.DurableEvents()) {
		t.Fatalf("new durable assistant did not advance anchor/cursor: %d -> %d", firstCursor, loop.pressureMeter.cursor)
	}
}

func TestNativeLoopProviderUsageAnchorTriggersPreRequestCompaction(t *testing.T) {
	s, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "usage-anchor-compaction", CreatedAt: 1}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativePressureModel{usage: &session.ModelUsage{InputTokens: 1080, OutputTokens: 20, TotalTokens: 1100}}
	loop, err := NewNativeLoop(s, model, "provider", "model", "Help the user.", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if _, err := loop.RunTextTurn(context.Background(), "Continue the earlier research."); err != nil {
			t.Fatal(err)
		}
	}
	if err := loop.SetContextPressure(1200, 32, 64); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "What next?"); err != nil {
		t.Fatal(err)
	}
	if model.summary == 0 || model.normal != 3 {
		t.Fatalf("provider-anchored pressure did not drive the third turn through compaction: normal=%d summary=%d", model.normal, model.summary)
	}
	if err := sessionformat.ValidateV4Relationships(s.DurableEvents()); err != nil {
		t.Fatal(err)
	}
}
