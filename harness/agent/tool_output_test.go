package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-bot/harness/content"
	"go-bot/harness/output"
	sessionformat "go-bot/harness/session"
	"go-bot/internal/modeldata"
)

type retainedProbe struct {
	normalizedProbe
	text string
}

func (p retainedProbe) ExecuteResult(context.Context, json.RawMessage) (modeldata.ToolResult, error) {
	return modeldata.ToolResult{ContentBlocks: []content.ContentBlock{{Type: "text", Text: p.text[:len(p.text)/2]}, {Type: "text", Text: p.text[len(p.text)/2:]}}, Meta: json.RawMessage(`{"source":"original"}`)}, nil
}
func TestNativeToolPostRetainsExactLongOutputInWorkspace(t *testing.T) {
	cwd := t.TempDir()
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "retained", CreatedAt: 1, CWD: cwd}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	original := strings.Repeat("Unicode output: 报告内容\n", 6000)
	loop, err := NewNativeLoop(live, &nativeAnswerModel{}, "p", "m", "core", []Tool{retainedProbe{text: original}}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	result := loop.executeNativeTool(t.Context(), 1, 1, modeldata.ToolRequest{ID: "spill", Name: "protocol_result", Arguments: `{}`})
	if result.IsError || output.Estimate(result.Content) > output.InlineTokens || !strings.Contains(result.Content, "Full formatted result stored at:") {
		t.Fatalf("retention failed: %+v", result)
	}
	files, err := filepath.Glob(filepath.Join(cwd, ".agent", "tool-spill", "*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("spill files: %v %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil || string(data) != original {
		t.Fatal("full output was not recoverable", err)
	}
	if string(result.Meta) != `{"source":"original"}` {
		t.Fatal("retention replaced tool metadata")
	}
}
func TestHardOutputLimitCoversMixedContentAndPreservesSmallImages(t *testing.T) {
	image := content.ImageBlock(content.ImageAttachment{ID: "sha256:" + strings.Repeat("0", 64), MIMEType: "image/png", Bytes: 10, Width: 2, Height: 2})
	small := modeldata.ToolResult{ContentBlocks: []content.ContentBlock{{Type: "text", Text: "before"}, image, {Type: "text", Text: "after"}}}
	if got := boundedToolResult(small); got.IsError || len(got.ContentBlocks) != 3 || got.ContentBlocks[1].ImageRefID != image.ImageRefID {
		t.Fatal("ordinary mixed result was damaged")
	}
	for _, failure := range []bool{false, true} {
		large := modeldata.ToolResult{IsError: failure, Content: "short prefix", ContentBlocks: []content.ContentBlock{{Type: "text", Text: strings.Repeat("大", maxDurableToolContentBytes/3)}, image, {Type: "text", Text: "tail"}}}
		got := boundedToolResult(large)
		if !got.IsError || len(got.ContentBlocks) != 0 || len(got.Content) > maxDurableToolErrorBytes+256 {
			t.Fatalf("unbounded mixed result survived: bytes=%d blocks=%d", len(got.Content), len(got.ContentBlocks))
		}
		if !failure && got.Code != "TOOL_OUTPUT_TOO_LARGE" {
			t.Fatal(got.Code)
		}
	}
}
