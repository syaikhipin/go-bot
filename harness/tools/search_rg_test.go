package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func requireRipgrep(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is required for the native discovery-tool test")
	}
	return path
}

func writeSearchFixture(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRipgrepGlobFindsHiddenIgnoredFilesButNotVCSMetadata(t *testing.T) {
	rg := requireRipgrep(t)
	root := t.TempDir()
	writeSearchFixture(t, root, ".gitignore", "ignored.txt\n")
	writeSearchFixture(t, root, "ignored.txt", "ignored")
	writeSearchFixture(t, root, ".hidden.txt", "hidden")
	writeSearchFixture(t, root, "nested/visible.txt", "visible")
	writeSearchFixture(t, root, ".git/config.txt", "must not appear")
	tool := RipgrepSearch{Name: "glob", Workdir: root, Binary: rg}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"*.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ignored.txt", ".hidden.txt", filepath.Join("nested", "visible.txt")} {
		if !strings.Contains(output, name) {
			t.Fatalf("glob omitted %s: %q", name, output)
		}
	}
	if strings.Contains(output, "config.txt") {
		t.Fatalf("glob entered VCS metadata: %q", output)
	}
}

func TestRipgrepGrepParsesLinesAndValidatesInclude(t *testing.T) {
	rg := requireRipgrep(t)
	root := t.TempDir()
	writeSearchFixture(t, root, "code/a.go", "first\nneedle here\n")
	writeSearchFixture(t, root, "code/b.txt", "needle excluded\n")
	tool := RipgrepSearch{Name: "grep", Workdir: root, Binary: rg}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"needle","include":"*.go"}`))
	if err != nil || !strings.Contains(output, "code/a.go\nLine 2: needle here") || strings.Contains(output, "b.txt") {
		t.Fatalf("grep output = %q, %v", output, err)
	}
	for _, raw := range []string{`{"pattern":"needle","include":""}`, `{"pattern":"needle","include":"!*.go"}`, `{"pattern":"needle","include":"*.go,*.txt"}`} {
		if err := tool.Validate(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted invalid include %s", raw)
		}
	}
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"pattern":"("}`))
	var searchErr *SearchToolError
	if !errors.As(err, &searchErr) || searchErr.Code != "SEARCH_INVALID_PATTERN" {
		t.Fatalf("invalid regex error = %v", err)
	}
}

func TestRipgrepSearchRetainsCompleteOverCapResultsInWorkspace(t *testing.T) {
	rg := requireRipgrep(t)
	root := t.TempDir()
	var lines strings.Builder
	for i := 0; i < 300; i++ {
		lines.WriteString("needle\n")
	}
	writeSearchFixture(t, root, "many.txt", lines.String())
	tool := RipgrepSearch{Name: "grep", Workdir: root, Binary: rg}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if err != nil || !strings.Contains(output, "Found 250 of 300 matches") {
		t.Fatalf("over-cap grep = %q, %v", output, err)
	}
	pathMatch := regexp.MustCompile(`Full result stored at: ([^\n]+?)\. Read the file`).FindStringSubmatch(output)
	if len(pathMatch) != 2 {
		t.Fatalf("grep did not expose spill path: %q", output)
	}
	spillPath := pathMatch[1]
	if !strings.HasPrefix(spillPath, filepath.Join(root, ".agent", "search-spill")+string(filepath.Separator)) {
		t.Fatalf("spill escaped workspace: %s", spillPath)
	}
	full, err := os.ReadFile(spillPath)
	if err != nil || strings.Count(string(full), "Line ") != 300 {
		t.Fatalf("complete spill has %d matches: %v", strings.Count(string(full), "Line "), err)
	}
}

func TestRipgrepSearchCancellationAndRawOutputBound(t *testing.T) {
	tool := RipgrepSearch{Name: "grep", Workdir: t.TempDir(), Binary: requireRipgrep(t)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tool.Execute(ctx, json.RawMessage(`{"pattern":"x"}`))
	var searchErr *SearchToolError
	if !errors.As(err, &searchErr) || searchErr.Code != "SEARCH_ABORTED" {
		t.Fatalf("cancelled search = %v", err)
	}
	var bounded boundedSearchOutput
	bounded.limit = 3
	if _, err := bounded.Write([]byte("1234")); err == nil || !bounded.overflow || bounded.buffer.Len() != 3 {
		t.Fatalf("raw output cap failed: %#v %v", bounded, err)
	}
}
