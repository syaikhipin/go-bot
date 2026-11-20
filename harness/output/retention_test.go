package output

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRecoverablePreviewPreservesUnicodeAndExactOriginal(t *testing.T) {
	root := t.TempDir()
	text := strings.Repeat("调研🙂结果\n", 12000)
	preview, err := RetainText(t.Context(), root, ".agent/tool-spill", "research", text, InlineTokens)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(preview) || Estimate(preview) > InlineTokens || !strings.Contains(preview, "Full formatted result stored at:") {
		t.Fatalf("invalid preview tokens=%d", Estimate(preview))
	}
	files, err := filepath.Glob(filepath.Join(root, ".agent", "tool-spill", "*.txt"))
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v error=%v", files, err)
	}
	full, err := os.ReadFile(files[0])
	if err != nil || string(full) != text || !strings.Contains(preview, files[0]) {
		t.Fatal("recovery path does not expose the exact original")
	}
	info, err := os.Stat(files[0])
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("spill is not private")
	}
	// Passing an already retained preview does not produce another spill copy.
	second, err := RetainText(t.Context(), root, ".agent/tool-spill", "research", preview, InlineTokens)
	if err != nil || second != preview {
		t.Fatal("retained preview was modified again")
	}
	files, _ = filepath.Glob(filepath.Join(root, ".agent", "tool-spill", "*.txt"))
	if len(files) != 1 {
		t.Fatal("output was double-spilled")
	}
}
func TestUnavailableRecoveryKeepsOutput(t *testing.T) {
	text := strings.Repeat("data", 20000)
	for _, ctx := range []context.Context{t.Context(), func() context.Context { ctx, cancel := context.WithCancel(t.Context()); cancel(); return ctx }()} {
		actual, err := RetainText(ctx, "/nonexistent/go-bot-output", ".agent/tool-spill", "tool", text, InlineTokens)
		if err == nil || actual != text {
			t.Fatal("missing recovery storage lost original content")
		}
	}
}
func TestJobTailRetainsStatusWithinByteBudget(t *testing.T) {
	text := strings.Repeat("运行🙂", 100)
	suffix := "\njob_id=known status=completed exit_code=0"
	got := FitTail(text, suffix, 120)
	if !utf8.ValidString(got) || len(got) > 120 || !strings.HasSuffix(got, suffix) || !strings.Contains(got, "truncated") {
		t.Fatalf("tail=%q", got)
	}
}
