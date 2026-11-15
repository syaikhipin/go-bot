package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-harness/harness/agent"
	"go-harness/internal/agentfs"
)

type failingMutationView struct {
	FileView
	err error
}

func (v failingMutationView) WriteFile(string, []byte) (string, error) {
	return "", v.err
}

func (v failingMutationView) EditFile(string, string, string, string, bool, int64) (string, error) {
	return "", v.err
}

func TestFileMutationUncertaintyReachesAgentLoop(t *testing.T) {
	for _, name := range []string{"writefile", "editfile"} {
		t.Run(name, func(t *testing.T) {
			view := failingMutationView{err: errors.Join(agentfs.ErrMutationOutcomeUnknown, errors.New("sync failed"))}
			list, err := NewFileTools(view)
			if err != nil {
				t.Fatal(err)
			}
			input := json.RawMessage(`{"path":"task.txt","content":"new"}`)
			if name == "editfile" {
				input = json.RawMessage(`{"path":"task.txt","old_string":"old","new_string":"new"}`)
			}
			if _, err := toolByName(t, list, name).Execute(context.Background(), input); !errors.Is(err, agent.ErrToolOutcomeUnknown) || !errors.Is(err, agentfs.ErrMutationOutcomeUnknown) {
				t.Fatalf("mutation error = %v; want uncertain outcome", err)
			}
		})
	}
	view := failingMutationView{err: agentfs.ErrChanged}
	list, err := NewFileTools(view)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := toolByName(t, list, "editfile").Execute(context.Background(), json.RawMessage(`{"path":"task.txt","old_string":"old","new_string":"new"}`)); !errors.Is(err, agentfs.ErrChanged) || errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Fatalf("pre-write version rejection misclassified: %v", err)
	}
}

func toolByName(t *testing.T, list []agent.Tool, name string) agent.Tool {
	t.Helper()
	for _, tool := range list {
		if tool.Spec().Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func TestFileToolsHonorContainerView(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	skillRoot := filepath.Join(workspace, ".agent", "skills")
	if err := os.MkdirAll(skillRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	view := agentfs.ContainerView{DefaultWorkdir: workspace}
	list, err := NewFileTools(view)
	if err != nil {
		t.Fatal(err)
	}
	write := toolByName(t, list, "writefile")
	input := json.RawMessage(`{"path":"note.txt","content":"first"}`)
	if err := write.Validate(input); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Execute(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	read := toolByName(t, list, "readfile")
	readInput, err := json.Marshal(map[string]string{"path": filepath.Join(workspace, "note.txt")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := read.Execute(context.Background(), readInput)
	if err != nil || !strings.Contains(got, "1: first") || !strings.Contains(got, "(End of file - total 1 lines)") {
		t.Fatalf("read = %q, %v", got, err)
	}
	search := toolByName(t, list, "searchfiles")
	searched, err := search.Execute(context.Background(), json.RawMessage(`{"pattern":"first","kind":"content"}`))
	if err != nil || !strings.Contains(searched, filepath.Join(workspace, "note.txt")) || !strings.Contains(searched, `"line":1`) {
		t.Fatalf("search = %q, %v", searched, err)
	}
	if err := search.Validate(json.RawMessage(`{"pattern":"("}`)); err == nil {
		t.Fatal("invalid search regex accepted")
	}
	skillInput, err := json.Marshal(map[string]string{"path": filepath.Join(skillRoot, "SKILL.md"), "content": "private copy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := write.Execute(context.Background(), skillInput); err != nil {
		t.Fatalf("private Skill write = %v", err)
	}
	outsideInput, err := json.Marshal(map[string]string{"path": filepath.Join(base, "outside.txt"), "content": "native access"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := write.Execute(context.Background(), outsideInput); err != nil {
		t.Fatalf("container-native absolute write = %v", err)
	}
	if err := toolByName(t, list, "editfile").Validate(json.RawMessage(`{"path":"note.txt","old_string":""}`)); err == nil {
		t.Fatal("empty exact-edit old text accepted")
	}
	if err := toolByName(t, list, "editfile").Validate(json.RawMessage(`{"path":"note.txt","old_string":"first","new_string":"first"}`)); err == nil {
		t.Fatal("no-op exact edit accepted")
	}
}

func TestFileToolsRejectTrailingJSONBeforeMutation(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "note.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := NewFileTools(agentfs.ContainerView{DefaultWorkdir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name string
		args string
	}{
		{name: "writefile", args: `{"path":"note.txt","content":"replacement"}{"path":"other.txt","content":"unexpected"}`},
		{name: "editfile", args: `{"path":"note.txt","old_string":"original","new_string":"replacement"}{"path":"other.txt"}`},
	} {
		t.Run(item.name, func(t *testing.T) {
			tool := toolByName(t, list, item.name)
			if err := tool.Validate(json.RawMessage(item.args)); err == nil {
				t.Fatal("accepted a second JSON argument value")
			}
			if _, err := tool.Execute(context.Background(), json.RawMessage(item.args)); err == nil {
				t.Fatal("executed a mutation with a second JSON argument value")
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "original" {
				t.Fatalf("invalid arguments changed the file: %q, %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(workspace, "other.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid arguments created the second file: %v", err)
			}
		})
	}
	read := toolByName(t, list, "readfile")
	if err := read.Validate(json.RawMessage(`{"path":"note.txt"} true`)); err == nil {
		t.Fatal("readfile accepted a trailing JSON value")
	}
	if err := read.Validate(json.RawMessage(`{"path":"note.txt"} trailing`)); err == nil {
		t.Fatal("readfile accepted trailing non-JSON content")
	}
}

func TestFileToolsRejectBlankPathBeforeMutation(t *testing.T) {
	workspace := t.TempDir()
	list, err := NewFileTools(agentfs.ContainerView{DefaultWorkdir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name string
		args string
	}{
		{name: "listfiles", args: `{"path":"   "}`},
		{name: "readfile", args: `{"path":"   "}`},
		{name: "writefile", args: `{"path":"   ","content":"unexpected"}`},
		{name: "editfile", args: `{"path":"   ","old_string":"old","new_string":"new"}`},
	} {
		t.Run(item.name, func(t *testing.T) {
			tool := toolByName(t, list, item.name)
			if err := tool.Validate(json.RawMessage(item.args)); err == nil {
				t.Fatal("accepted a whitespace-only path")
			}
			if _, err := tool.Execute(context.Background(), json.RawMessage(item.args)); err == nil {
				t.Fatal("executed with a whitespace-only path")
			}
		})
	}
	if _, err := os.Stat(filepath.Join(workspace, "   ")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blank-path tool created a file: %v", err)
	}
}

func TestObservedFileToolsGuardExistingFileAcrossToolCalls(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "existing.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := NewObservedFileTools(agentfs.ContainerView{DefaultWorkdir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	write := toolByName(t, list, "writefile")
	edit := toolByName(t, list, "editfile")
	read := toolByName(t, list, "readfile")
	if _, err := write.Execute(context.Background(), json.RawMessage(`{"path":"existing.txt","content":"blind"}`)); !errors.Is(err, agentfs.ErrNotObserved) {
		t.Fatalf("blind overwrite = %v", err)
	}
	if _, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"existing.txt","old_string":"old","new_string":"blind"}`)); !errors.Is(err, agentfs.ErrNotObserved) {
		t.Fatalf("blind edit = %v", err)
	}
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"path":"existing.txt"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"existing.txt","old_string":"old","new_string":"new"}`)); err != nil {
		t.Fatalf("edit after read = %v", err)
	}
	if _, err := write.Execute(context.Background(), json.RawMessage(`{"path":"existing.txt","content":"final"}`)); err != nil {
		t.Fatalf("write after edit = %v", err)
	}
}

func TestObservedFileToolsRejectedBinaryReadDoesNotAuthorizeOverwrite(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "binary.dat")
	if err := os.WriteFile(path, []byte{0, 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := NewObservedFileTools(agentfs.ContainerView{DefaultWorkdir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := toolByName(t, list, "readfile").Execute(context.Background(), json.RawMessage(`{"path":"binary.dat"}`)); err == nil {
		t.Fatal("binary file was accepted as UTF-8 text")
	}
	if _, err := toolByName(t, list, "writefile").Execute(context.Background(), json.RawMessage(`{"path":"binary.dat","content":"blind"}`)); !errors.Is(err, agentfs.ErrNotObserved) {
		t.Fatalf("rejected binary read authorized overwrite: %v", err)
	}
}

func TestFileToolsDoNotMutateAfterRequestCancellation(t *testing.T) {
	workspace := t.TempDir()
	view := agentfs.ContainerView{DefaultWorkdir: workspace}
	if _, err := view.WriteFile("note.txt", []byte("original")); err != nil {
		t.Fatal(err)
	}
	list, err := NewFileTools(view)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, item := range []struct {
		name string
		args string
	}{
		{name: "writefile", args: `{"path":"note.txt","content":"replacement"}`},
		{name: "editfile", args: `{"path":"note.txt","old_string":"original","new_string":"replacement"}`},
	} {
		if _, err := toolByName(t, list, item.name).Execute(ctx, json.RawMessage(item.args)); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s cancellation = %v", item.name, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "note.txt")); err != nil || string(got) != "original" {
		t.Fatalf("canceled tools modified file: %q, %v", got, err)
	}
}

func TestReadFileTruncatesOneHugeLineWithoutLosingTotalLines(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "large.txt"), []byte(strings.Repeat("a", 1<<20+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	view := agentfs.ContainerView{DefaultWorkdir: workspace}
	list, err := NewFileTools(view)
	if err != nil {
		t.Fatal(err)
	}
	result, err := toolByName(t, list, "readfile").Execute(context.Background(), json.RawMessage(`{"path":"large.txt"}`))
	if err != nil || !strings.Contains(result, "... (line truncated to 2000 chars)") ||
		!strings.Contains(result, "(End of file - total 1 lines)") || len(result) > 3000 {
		t.Fatalf("large line read = %q, %v", result[:min(len(result), 120)], err)
	}
}

func TestReadFileContinuesByLineWithUnicodeAndCRLF(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "text.txt"), []byte("abc世x\r\nsecond\nthird"), 0o600); err != nil {
		t.Fatal(err)
	}
	view := agentfs.ContainerView{DefaultWorkdir: workspace}
	list, err := NewFileTools(view)
	if err != nil {
		t.Fatal(err)
	}
	read := toolByName(t, list, "readfile")
	first, err := read.Execute(context.Background(), json.RawMessage(`{"path":"text.txt","limit":1}`))
	if err != nil || !strings.Contains(first, "1: abc世x\n\n(Showing lines 1-1 of 3. Use offset=2 to continue.)") {
		t.Fatalf("first window = %q, %v", first, err)
	}
	second, err := read.Execute(context.Background(), json.RawMessage(`{"path":"text.txt","offset":2,"limit":2}`))
	if err != nil || !strings.Contains(second, "2: second\n3: third\n\n(End of file - total 3 lines)") {
		t.Fatalf("second window = %q, %v", second, err)
	}
	for _, bad := range []string{`{"path":"text.txt","offset":0}`, `{"path":"text.txt","limit":2001}`, `{"path":"text.txt","offset_bytes":3}`} {
		if err := read.Validate(json.RawMessage(bad)); err == nil {
			t.Fatalf("invalid read window was accepted: %s", bad)
		}
	}
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"path":"text.txt","offset":4}`)); err == nil {
		t.Fatal("out-of-range line offset was accepted")
	}
}

func TestReadFileByteCapStillReportsExactTotalLines(t *testing.T) {
	workspace := t.TempDir()
	line := strings.Repeat("x", 2000) + "\n"
	if err := os.WriteFile(filepath.Join(workspace, "many.txt"), []byte(strings.Repeat(line, 30)), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := NewFileTools(agentfs.ContainerView{DefaultWorkdir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	read := toolByName(t, list, "readfile")
	first, err := read.Execute(context.Background(), json.RawMessage(`{"path":"many.txt"}`))
	if err != nil || !strings.Contains(first, "25: "+strings.Repeat("x", 2000)) ||
		strings.Contains(first, "26: ") || !strings.Contains(first, "(Output capped. Showing lines 1-25. Use offset=26 to continue.)") {
		t.Fatalf("byte-capped window = %q, %v", first[:min(len(first), 150)], err)
	}
	second, err := read.Execute(context.Background(), json.RawMessage(`{"path":"many.txt","offset":26}`))
	if err != nil || !strings.Contains(second, "26: ") || !strings.Contains(second, "(End of file - total 30 lines)") {
		t.Fatalf("continued window = %q, %v", second[:min(len(second), 150)], err)
	}
}

func TestReadFileRejectsInvalidTextBeyondReturnedWindowWithoutObserving(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "mixed.txt"), []byte{'o', 'k', '\n', 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := NewObservedFileTools(agentfs.ContainerView{DefaultWorkdir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := toolByName(t, list, "readfile").Execute(context.Background(), json.RawMessage(`{"path":"mixed.txt","limit":1}`)); err == nil {
		t.Fatal("invalid UTF-8 after the displayed window was accepted")
	}
	if _, err := toolByName(t, list, "writefile").Execute(context.Background(), json.RawMessage(`{"path":"mixed.txt","content":"replacement"}`)); !errors.Is(err, agentfs.ErrNotObserved) {
		t.Fatalf("failed read authorized overwrite: %v", err)
	}
}

func TestReadFileLineLengthCountsUTF16Units(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "emoji.txt"), []byte(strings.Repeat("😀", 1001)), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := NewFileTools(agentfs.ContainerView{DefaultWorkdir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	result, err := toolByName(t, list, "readfile").Execute(context.Background(), json.RawMessage(`{"path":"emoji.txt"}`))
	if err != nil || !strings.Contains(result, "1: "+strings.Repeat("😀", 1000)+"... (line truncated to 2000 chars)") ||
		strings.Contains(result, strings.Repeat("😀", 1001)) {
		t.Fatalf("UTF-16 line cap = %q, %v", result[:min(len(result), 120)], err)
	}
}

func TestReadFileIgnoresInitialUTF8BOM(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "bom.txt"), []byte("\ufefffirst\nsecond"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := NewFileTools(agentfs.ContainerView{DefaultWorkdir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	result, err := toolByName(t, list, "readfile").Execute(context.Background(), json.RawMessage(`{"path":"bom.txt"}`))
	if err != nil || !strings.Contains(result, "1: first\n2: second") || strings.Contains(result, "\ufeff") {
		t.Fatalf("BOM-prefixed read = %q, %v", result, err)
	}
}
