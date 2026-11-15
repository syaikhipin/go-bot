package instructionrender

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func baselineSection(file File) string {
	return "Instructions from: " + file.Path + "\n\n" + file.Content
}

func TestRenderReplacementCanOmitFileThatFitsNormalBaseline(t *testing.T) {
	var files []File
	for size := 65000; size < 65536; size++ {
		files = []File{{Path: "AGENTS.md", Content: strings.Repeat("x", size)}, {Path: "CLAUDE.md", Content: "specific"}}
		normal := Render(files, 65536, BaselineIntro, baselineSection)
		replacement := Render(files, 65536, ReplacementIntro, baselineSection)
		if reflect.DeepEqual(normal.Represented, []int{0, 1}) && reflect.DeepEqual(replacement.Represented, []int{1}) {
			if !strings.Contains(replacement.Text, "omitted AGENTS.md") || strings.Contains(replacement.Text, strings.Repeat("x", 100)) ||
				len(replacement.Text) > 65536 {
				t.Fatalf("replacement budget: %q", replacement.Text)
			}
			return
		}
	}
	t.Fatal("no fixture separated normal and replacement source sets")
}

func TestRenderTruncationIsUTF8AndRecordsExactSource(t *testing.T) {
	files := []File{{Path: "AGENTS.md", Content: "broad"}, {Path: "nested/CLAUDE.md", Content: strings.Repeat("界", 65536)}}
	rendered := Render(files, 65536, BaselineIntro, baselineSection)
	if !reflect.DeepEqual(rendered.Represented, []int{1}) || len(rendered.Text) > 65536 || !utf8.ValidString(rendered.Text) ||
		!strings.Contains(rendered.Text, "omitted AGENTS.md; truncated nested/CLAUDE.md from 196608 to ") {
		t.Fatalf("truncated source accounting: bytes=%d, represented=%v, text=%q", len(rendered.Text), rendered.Represented, rendered.Text[:min(300, len(rendered.Text))])
	}
}

func TestRenderDoesNotRepresentNoticeOnlyContent(t *testing.T) {
	files := []File{{Path: strings.Repeat("p", 80), Content: "must not be marked seen"}}
	rendered := Render(files, 48, BaselineIntro, baselineSection)
	if len(rendered.Text) > 48 || !utf8.ValidString(rendered.Text) || len(rendered.Represented) != 0 {
		t.Fatalf("notice-only source was represented: %+v", rendered)
	}
	empty := Render([]File{{Path: "AGENTS.md"}}, 65536, BaselineIntro, baselineSection)
	if !reflect.DeepEqual(empty.Represented, []int{0}) || !strings.Contains(empty.Text, "Instructions from: AGENTS.md") {
		t.Fatalf("genuinely empty file was omitted: %+v", empty)
	}
}
