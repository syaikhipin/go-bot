package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"go-harness/harness/session"
	"go-harness/internal/testsupport/session"
	"go-harness/thirdparty/llm"
)

type nativeCompactionModel struct {
	normalCalls         int
	summaryCalls        int
	normalRequests      [][]Message
	failSummary         bool
	maxTokensSummary    bool
	badUsage            bool
	summaryNeedsOffload bool
	panicSummary        bool
}

func (m *nativeCompactionModel) Complete(_ context.Context, messages []Message, _ []ToolSpec) (session.AssistantMessage, error) {
	if len(messages) > 0 && messages[len(messages)-1].Content == nativeCompactionInstruction {
		m.summaryCalls++
		if m.panicSummary {
			panic("summary model crash")
		}
		if m.summaryNeedsOffload {
			for _, message := range messages {
				imageIndex := 0
				for _, block := range message.ContentBlocks {
					if block.Type != "image" {
						continue
					}
					if !message.IsImageOffloaded(imageIndex) {
						return session.AssistantMessage{}, &ImageOffloadRequiredError{Count: 1}
					}
					imageIndex++
				}
			}
		}
		if m.failSummary {
			return session.AssistantMessage{ToolCalls: []session.ToolRequest{{ID: "bad", Name: "echo", Arguments: `{}`}}}, nil
		}
		if m.maxTokensSummary {
			return session.AssistantMessage{Content: "incomplete checkpoint", MaxTokens: true}, nil
		}
		if m.badUsage {
			return session.AssistantMessage{Content: "summary", Usage: &session.ModelUsage{InputTokens: 1, CachedInputTokens: 2}}, nil
		}
		return session.AssistantMessage{Content: "## Primary Request and Intent\n- Continue the task.\n\n## Next Step\n- Finish it.",
			RawOutput: []session.ContentBlock{{Type: "text", Text: "## Primary Request and Intent\n- Continue the task."},
				{Type: "text", Text: "\n\n## Next Step\n- Finish it."}}}, nil
	}
	m.normalCalls++
	m.normalRequests = append(m.normalRequests, append([]Message(nil), messages...))
	if m.normalCalls <= 3 {
		return session.AssistantMessage{Content: "ack"}, nil
	}
	for _, message := range messages {
		if strings.Contains(message.Content, "<compacted-summary>") {
			return session.AssistantMessage{Content: "recovered"}, nil
		}
	}
	return session.AssistantMessage{}, &llm.ModelError{Code: llm.ModelErrorContextLimitExceeded, Cause: errors.New("maximum context length exceeded")}
}

func TestNativeInstructionsRebuildAfterCompactionRemovesBaseline(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "instruction-after-compaction", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeCompactionModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	guidance := NativeInstructionSnapshot{
		Identity: "project", Text: "<system-reminder>\nInstructions from: AGENTS.md\n\ncurrent guidance\n</system-reminder>",
		Changes: []NativeInstructionChange{{Action: "set", Scope: ".\x00AGENTS.md", Path: "AGENTS.md", Digest: "current"}},
		Content: map[string]string{".\x00AGENTS.md": "current guidance"},
	}
	loop.SetInstructionProvider(func(context.Context, []string) (NativeInstructionSnapshot, error) { return guidance, nil })
	for index := 0; index < 3; index++ {
		if answer, err := loop.RunTextTurn(context.Background(), strings.Repeat("a", 1600)); err != nil || answer != "ack" {
			t.Fatalf("seed turn %d = %q, %v", index, answer, err)
		}
	}
	if compacted, err := loop.CompactNow(context.Background()); err != nil || !compacted {
		t.Fatalf("compact = %t, %v", compacted, err)
	}
	visible, err := loop.visibleInstructions()
	if err != nil || visible.baseline {
		t.Fatalf("old instruction baseline unexpectedly survived compaction: %+v, %v", visible, err)
	}
	if answer, err := loop.RunTextTurn(context.Background(), "continue with guidance"); err != nil || answer != "recovered" {
		t.Fatalf("post-compaction turn = %q, %v", answer, err)
	}
	if len(model.normalRequests) != 4 {
		t.Fatalf("model request count = %d", len(model.normalRequests))
	}
	var sawGuidance, sawCheckpoint bool
	for _, message := range model.normalRequests[3] {
		sawGuidance = sawGuidance || strings.Contains(message.Content, "current guidance")
		sawCheckpoint = sawCheckpoint || strings.Contains(message.Content, "<compacted-summary>")
	}
	if !sawGuidance || !sawCheckpoint {
		t.Fatalf("resumed model request: guidance=%t checkpoint=%t", sawGuidance, sawCheckpoint)
	}
	count := 0
	for _, event := range s.DurableEvents() {
		if event.Type == "user/message" && strings.Contains(string(event.Data), `"kind":"agent-instructions"`) {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("instruction baseline append count = %d, want original and rebuilt", count)
	}
}

func TestNativeInstructionsRebuildBeforeOverflowRetry(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "instruction-overflow-retry", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeCompactionModel{}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	guidance := NativeInstructionSnapshot{
		Identity: "project", Text: "<system-reminder>\nInstructions from: AGENTS.md\n\nretry guidance\n</system-reminder>",
		Changes: []NativeInstructionChange{{Action: "set", Scope: ".\x00AGENTS.md", Path: "AGENTS.md", Digest: "retry"}},
		Content: map[string]string{".\x00AGENTS.md": "retry guidance"},
	}
	loop.SetInstructionProvider(func(context.Context, []string) (NativeInstructionSnapshot, error) { return guidance, nil })
	for index := 0; index < 3; index++ {
		if answer, err := loop.RunTextTurn(context.Background(), strings.Repeat("a", 1600)); err != nil || answer != "ack" {
			t.Fatalf("seed turn %d = %q, %v", index, answer, err)
		}
	}
	if answer, err := loop.RunTextTurn(context.Background(), "trigger overflow"); err != nil || answer != "recovered" {
		t.Fatalf("overflow recovery = %q, %v", answer, err)
	}
	if len(model.normalRequests) != 5 {
		t.Fatalf("normal requests = %d", len(model.normalRequests))
	}
	var sawGuidance, sawCheckpoint bool
	for _, message := range model.normalRequests[4] {
		sawGuidance = sawGuidance || strings.Contains(message.Content, "retry guidance")
		sawCheckpoint = sawCheckpoint || strings.Contains(message.Content, "<compacted-summary>")
	}
	if !sawGuidance || !sawCheckpoint {
		t.Fatalf("overflow retry: guidance=%t checkpoint=%t", sawGuidance, sawCheckpoint)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
}

func nativeCompactionLoop(t *testing.T, model *nativeCompactionModel) (*NativeLoop, *sessionformat.LiveSession) {
	t.Helper()
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-summary", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		answer, err := loop.RunTextTurn(context.Background(), strings.Repeat(string(rune('a'+index)), 1600))
		if err != nil || answer != "ack" {
			t.Fatalf("seed turn %d = %q, %v", index, answer, err)
		}
	}
	return loop, s
}

func TestNativeLoopContextOverflowWritesDeepSeekCompactionTransaction(t *testing.T) {
	model := &nativeCompactionModel{}
	loop, s := nativeCompactionLoop(t, model)
	answer, err := loop.RunTextTurn(context.Background(), "final question")
	if err != nil || answer != "recovered" || model.summaryCalls != 1 || model.normalCalls != 5 {
		t.Fatalf("summary recovery = %q, %v; summary=%d normal=%d", answer, err, model.summaryCalls, model.normalCalls)
	}
	events := s.Events()
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Type]++
	}
	if counts["compaction/start"] != 1 || counts["compaction/summary"] != 1 || counts["compaction/end"] != 1 || counts["llm/retry"] != 0 || counts["assistant/attempt"] != 1 {
		t.Fatalf("wrong v4 compaction transaction: %+v", counts)
	}
	for _, event := range events {
		if event.Type != "compaction/summary" {
			continue
		}
		var data struct {
			RawOutput []session.ContentBlock `json:"rawOutput"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil || len(data.RawOutput) != 2 ||
			data.RawOutput[0].Text != "## Primary Request and Intent\n- Continue the task." ||
			bytes.Contains(event.Data, []byte(`"llmStreamCall"`)) {
			t.Fatalf("incorrect non-stream v4 summary record: %s, %v", event.Data, err)
		}
	}
	var checkpoint sessionformat.Event
	for _, event := range events {
		if event.Type == "user/message" && strings.Contains(string(event.Data), "compact-checkpoint") {
			checkpoint = event
			break
		}
	}
	if checkpoint.Type == "" || len(checkpoint.SourceEventSeqs) == 0 || !strings.Contains(string(checkpoint.SurfaceOp), `"replace"`) {
		t.Fatalf("checkpoint is not a source-cited replacement: %+v", checkpoint)
	}
	if err := sessionformat.ValidateV4Relationships(events); err != nil {
		t.Fatal(err)
	}
	surface, err := sessionformat.FoldSurface(events)
	if err != nil {
		t.Fatal(err)
	}
	var checkpointVisible, finalQuestionVisible, oldHistoryVisible bool
	for _, raw := range surface.Messages {
		var message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatal(err)
		}
		for _, block := range message.Content {
			checkpointVisible = checkpointVisible || strings.Contains(block.Text, "<compacted-summary>")
			finalQuestionVisible = finalQuestionVisible || block.Text == "final question"
			oldHistoryVisible = oldHistoryVisible || block.Text == strings.Repeat("a", 1600)
		}
	}
	if !checkpointVisible || !finalQuestionVisible || oldHistoryVisible {
		t.Fatalf("wrong restored model surface: checkpoint=%v final=%v old=%v", checkpointVisible, finalQuestionVisible, oldHistoryVisible)
	}
	var artifact bytes.Buffer
	header, err := sessionformat.EncodeHeader(s.Header())
	if err != nil {
		t.Fatal(err)
	}
	artifact.Write(header)
	artifact.WriteByte('\n')
	for index, event := range events {
		line, err := sessionformat.EncodeEvent(event, uint64(index))
		if err != nil {
			t.Fatal(err)
		}
		artifact.Write(line)
		artifact.WriteByte('\n')
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_COMPACTION_EXPORT_PATH"); exportPath != "" {
		if err := os.WriteFile(exportPath, artifact.Bytes(), 0o600); err != nil {
			t.Fatalf("export native v4 compaction journal: %v", err)
		}
	}
	inspection, err := sessionformat.InspectV4JSONL(&artifact)
	if err != nil {
		t.Fatalf("completed native compaction cannot be cold-read as v4: %v", err)
	}
	restoredJournal := &nativeTestJournal{events: append([]sessionformat.Event(nil), events...)}
	restored, err := sessionformat.RestoreLiveSession(inspection.PhysicalArtifact, restoredJournal)
	if err != nil {
		t.Fatalf("completed native compaction cannot be adopted on resume: %v", err)
	}
	resumeModel := &nativeCompactionModel{normalCalls: 3}
	resumeLoop, err := NewNativeLoop(restored, resumeModel, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := resumeLoop.RunTextTurn(context.Background(), "continue"); err != nil || answer != "recovered" {
		t.Fatalf("resumed checkpoint turn = %q, %v", answer, err)
	}
	if err := sessionformat.ValidateV4Relationships(restored.Events()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeLoopManualCompactionIsStandaloneAndReplayable(t *testing.T) {
	model := &nativeCompactionModel{}
	loop, s := nativeCompactionLoop(t, model)
	compacted, err := loop.CompactNow(context.Background())
	if err != nil || !compacted || model.summaryCalls != 1 {
		t.Fatalf("manual compaction = %t, %v; summaries=%d", compacted, err, model.summaryCalls)
	}
	events := s.Events()
	var commandID string
	var starts, summaries, ends, checkpoints int
	for _, event := range events {
		switch event.Type {
		case "command/run":
			var data struct{ CommandID, Name string }
			if err := json.Unmarshal(event.Data, &data); err != nil || data.Name != "compact" {
				t.Fatalf("command/run = %s, %v", event.Data, err)
			}
			commandID = data.CommandID
		case "compaction/start", "compaction/end":
			var data struct {
				Turn            *uint64 `json:"turn"`
				SourceCommandID string  `json:"sourceCommandId"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil || data.Turn != nil || data.SourceCommandID != commandID {
				t.Fatalf("standalone marker %s = %s, %v", event.Type, event.Data, err)
			}
			if event.Type == "compaction/start" {
				starts++
			} else {
				ends++
			}
		case "compaction/summary":
			summaries++
		case "user/message":
			if strings.Contains(string(event.Data), `"compact-checkpoint"`) {
				checkpoints++
			}
		}
	}
	if commandID == "" || starts != 1 || summaries != 1 || ends != 1 || checkpoints != 1 {
		t.Fatalf("manual command trace: command=%q start=%d summary=%d end=%d checkpoint=%d", commandID, starts, summaries, ends, checkpoints)
	}
	if err := sessionformat.ValidateV4Relationships(events); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Surface(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeLoopFailedSummaryClosesCompactionWithoutReplacingSurface(t *testing.T) {
	model := &nativeCompactionModel{failSummary: true}
	loop, s := nativeCompactionLoop(t, model)
	if _, err := loop.RunTextTurn(context.Background(), "final question"); err == nil {
		t.Fatal("failed summary was accepted")
	}
	counts := map[string]int{}
	for _, event := range s.Events() {
		counts[event.Type]++
	}
	if counts["compaction/start"] != 1 || counts["compaction/end"] != 1 || counts["compaction/summary"] != 0 {
		t.Fatalf("failed summary did not close its bracket: %+v", counts)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
	surface, err := s.Surface()
	if err != nil {
		t.Fatal(err)
	}
	if len(surface.Replacements) != 0 {
		t.Fatal("failed summary changed the model surface")
	}
}

func TestNativeLoopMaxTokensSummaryCannotBecomeCheckpoint(t *testing.T) {
	model := &nativeCompactionModel{maxTokensSummary: true}
	loop, s := nativeCompactionLoop(t, model)
	if _, err := loop.RunTextTurn(context.Background(), "final question"); err == nil || !strings.Contains(err.Error(), "truncated at the token cap") {
		t.Fatalf("truncated summary was accepted or misclassified: %v", err)
	}
	counts := map[string]int{}
	for _, event := range s.Events() {
		counts[event.Type]++
	}
	if counts["compaction/start"] != 1 || counts["compaction/end"] != 1 || counts["compaction/summary"] != 0 {
		t.Fatalf("truncated summary changed compaction surface: %+v", counts)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeLoopInvalidSummaryUsageClosesCompaction(t *testing.T) {
	model := &nativeCompactionModel{badUsage: true}
	loop, s := nativeCompactionLoop(t, model)
	if _, err := loop.RunTextTurn(context.Background(), "final question"); err == nil {
		t.Fatal("invalid auxiliary model usage was accepted")
	}
	counts := map[string]int{}
	for _, event := range s.Events() {
		counts[event.Type]++
	}
	if counts["compaction/start"] != 1 || counts["compaction/end"] != 1 || counts["compaction/summary"] != 0 {
		t.Fatalf("invalid summary usage left an open compaction: %+v", counts)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeLoopSummaryPanicClosesCompaction(t *testing.T) {
	model := &nativeCompactionModel{panicSummary: true}
	loop, s := nativeCompactionLoop(t, model)
	if _, err := loop.RunTextTurn(context.Background(), "final question"); err == nil {
		t.Fatal("panicked summarizer was accepted")
	}
	counts := map[string]int{}
	for _, event := range s.Events() {
		counts[event.Type]++
	}
	if counts["compaction/start"] != 1 || counts["compaction/end"] != 1 || counts["compaction/summary"] != 0 {
		t.Fatalf("summary panic left an open compaction: %+v", counts)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSummaryOffloadsOnlyImagesInsideSelectedRegion(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-summary-image", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeCompactionModel{summaryNeedsOffload: true}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	oldImage := json.RawMessage(`{"id":"old-image","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"` + strings.Repeat("a", 1600) + `"},{"type":"image","attachment":{"attachmentId":"image-old","mediaType":"image/png","bytes":10}}]}`)
	if _, err := loop.AcceptMessage(context.Background(), oldImage); err != nil {
		t.Fatal(err)
	}
	if ran, _, err := loop.RunNext(context.Background()); err != nil || !ran {
		t.Fatalf("old image turn = %v, %v", ran, err)
	}
	for _, text := range []string{strings.Repeat("b", 1600), strings.Repeat("c", 1600)} {
		if _, err := loop.RunTextTurn(context.Background(), text); err != nil {
			t.Fatal(err)
		}
	}
	newImage := json.RawMessage(`{"id":"new-image","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"final question"},{"type":"image","attachment":{"attachmentId":"image-new","mediaType":"image/png","bytes":10}}]}`)
	if _, err := loop.AcceptMessage(context.Background(), newImage); err != nil {
		t.Fatal(err)
	}
	if ran, answer, err := loop.RunNext(context.Background()); err != nil || !ran || answer != "recovered" {
		t.Fatalf("image summary recovery = %v, %q, %v", ran, answer, err)
	}
	if model.summaryCalls != 2 {
		t.Fatalf("summary image offload did not retry exactly once: %d", model.summaryCalls)
	}
	var oldSeq, newSeq uint64
	var target imageOffloadEvent
	for _, event := range s.Events() {
		if event.Type == "user/message" && strings.Contains(string(event.Data), "image-old") {
			oldSeq = event.Seq
		}
		if event.Type == "user/message" && strings.Contains(string(event.Data), "image-new") {
			newSeq = event.Seq
		}
		if event.Type == "image/offload" {
			if err := json.Unmarshal(event.Data, &target); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(target.Targets) != 1 || target.Targets[0].Seq != oldSeq || target.Targets[0].Seq == newSeq || len(target.Targets[0].ImageIndexes) != 1 || target.Targets[0].ImageIndexes[0] != 0 {
		t.Fatalf("summary offloaded a current image outside its region: old=%d new=%d targets=%+v", oldSeq, newSeq, target.Targets)
	}
	if err := sessionformat.ValidateV4Relationships(s.Events()); err != nil {
		t.Fatal(err)
	}
	surface, err := s.Surface()
	if err != nil {
		t.Fatal(err)
	}
	for index, seq := range surface.MessageSeqs {
		if seq == newSeq && strings.Contains(string(surface.Messages[index]), `"offloaded":true`) {
			t.Fatal("new image outside the summary region was offloaded")
		}
	}
}

func TestNativeCompactionRangeKeepsToolCallResultPairTogether(t *testing.T) {
	messages := []json.RawMessage{
		json.RawMessage(`{"role":"system","content":[{"type":"text","text":"prompt"}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"old"}]}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"tool-call","id":"call","name":"echo","arguments":"{}"}]}`),
		json.RawMessage(`{"role":"tool","content":[{"type":"text","text":"result"}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"new"}]}`),
	}
	surface := sessionformat.Surface{Nodes: []uint64{0, 1, 2, 3, 4}, MessageSeqs: []uint64{0, 1, 2, 3, 4}, Messages: messages}
	events := []sessionformat.Event{
		{Seq: 0, Type: "system/message"}, {Seq: 1, Type: "user/message"},
		{Seq: 2, Type: "assistant/message", Data: json.RawMessage(`{"message":{"content":[{"type":"tool-call","id":"call","name":"echo","arguments":"{}"}]}}`)},
		{Seq: 3, Type: "tool/result"}, {Seq: 4, Type: "user/message"},
	}
	lastPrice, err := estimateNativeMessage(messages[4])
	if err != nil {
		t.Fatal(err)
	}
	resultPrice, err := estimateNativeMessage(messages[3])
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectNativeCompactRange(surface, events, lastPrice+resultPrice, nil)
	if err != nil || selected == nil || len(selected.seqs) != 1 || selected.seqs[0] != 1 {
		t.Fatalf("compaction split tool pair: %+v, %v", selected, err)
	}
}

func TestNativeCompactionRetentionAndShrinkUseFileRequestPrice(t *testing.T) {
	ref := NativeFileAttachmentRef{ID: "sha256:" + strings.Repeat("b", 64), Name: "notes.txt", Bytes: 10}
	file, err := json.Marshal(map[string]any{"role": "user", "content": []any{map[string]any{
		"type": "file", "attachment": map[string]any{"attachmentId": ref.ID, "name": ref.Name, "bytes": ref.Bytes},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	surface := sessionformat.Surface{
		Nodes: []uint64{0, 1, 2, 3}, MessageSeqs: []uint64{0, 1, 2, 3},
		Messages: []json.RawMessage{
			json.RawMessage(`{"role":"system","content":[{"type":"text","text":"prompt"}]}`),
			json.RawMessage(`{"role":"user","content":[{"type":"text","text":"earlier work"}]}`),
			file,
			json.RawMessage(`{"role":"user","content":[{"type":"text","text":"continue"}]}`),
		},
	}
	events := []sessionformat.Event{{Seq: 0, Type: "system/message"}, {Seq: 1, Type: "user/message"}, {Seq: 2, Type: "user/message"}, {Seq: 3, Type: "user/message"}}
	resolve := func(NativeFileAttachmentRef) string { return "/root/harness/" + strings.Repeat("long-path/", 300) }
	selected, err := selectNativeCompactRange(surface, events, 100, resolve)
	if err != nil || selected == nil || len(selected.seqs) != 1 || selected.seqs[0] != 1 {
		t.Fatalf("route-priced retention failed to keep the file handle: %+v, %v", selected, err)
	}
	selected, err = selectNativeCompactRange(surface, events, 1, resolve)
	if err != nil || selected == nil || len(selected.seqs) != 2 || selected.seqs[0] != 1 || selected.seqs[1] != 2 || selected.routeTokens <= selected.tokens {
		t.Fatalf("file span did not retain distinct route/shadow prices: %+v, %v", selected, err)
	}
}

func TestNativeLoopContextOverflowWithNoCompactableHistoryStops(t *testing.T) {
	header := sessionformat.Header{Type: "session", Version: 4, ID: "native-summary-none", CreatedAt: 1}
	s, err := sessionformat.NewLiveSession(header, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	model := &nativeCompactionModel{normalCalls: 3}
	loop, err := NewNativeLoop(s, model, "provider", "model", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunTextTurn(context.Background(), "first question"); err == nil || model.summaryCalls != 0 || model.normalCalls != 4 {
		t.Fatalf("non-compactable overflow = %v, normal=%d summary=%d", err, model.normalCalls, model.summaryCalls)
	}
}
