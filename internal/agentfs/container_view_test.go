package agentfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestContainerViewUsesNativePathsAndDefaultWorkdir(t *testing.T) {
	workspace := t.TempDir()
	other := t.TempDir()
	view := ContainerView{DefaultWorkdir: workspace}
	if _, err := view.WriteFile("relative.txt", []byte("alpha")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "relative.txt")); err != nil || string(data) != "alpha" {
		t.Fatalf("relative write = %q %v", data, err)
	}
	if _, err := view.WriteFile("reports/2026/result.txt", []byte("nested")); err != nil {
		t.Fatalf("nested container write: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "reports", "2026", "result.txt")); err != nil || string(data) != "nested" {
		t.Fatalf("nested container result = %q %v", data, err)
	}
	absolute := filepath.Join(other, "outside-workspace.txt")
	if _, err := view.WriteFile(absolute, []byte("before")); err != nil {
		t.Fatal(err)
	}
	read, err := view.ReadFile(absolute, 1024)
	if err != nil || string(read.Content) != "before" {
		t.Fatalf("absolute read = %#v %v", read, err)
	}
	if _, err := view.EditFile(absolute, "before", "after", read.SHA256, false, 1024); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(absolute); err != nil || string(data) != "after" {
		t.Fatalf("absolute edit = %q %v", data, err)
	}
	if _, err := view.ReadFile("", 1024); err == nil {
		t.Fatal("empty path accepted")
	}
}

func TestContainerViewPartialReadUsesCompleteFileDigestOnlyWhenAvailable(t *testing.T) {
	view := ContainerView{DefaultWorkdir: t.TempDir()}
	content := []byte(strings.Repeat("prefix", 32) + " unique-tail")
	if _, err := view.WriteFile("note.txt", content); err != nil {
		t.Fatal(err)
	}
	read, err := view.ReadFile("note.txt", 16)
	if err != nil || !read.Truncated || read.SHA256 != digest(content) || string(read.Content) != string(content[:16]) {
		t.Fatalf("partial read = %#v, %v", read, err)
	}
	if _, err := view.EditFile("note.txt", "unique-tail", "edited-tail", read.SHA256, false, 1<<20); err != nil {
		t.Fatalf("complete digest from a partial read did not authorize exact edit: %v", err)
	}
	if _, err := view.WriteFile("large.txt", []byte(strings.Repeat("a", 1<<20+1))); err != nil {
		t.Fatal(err)
	}
	large, err := view.ReadFile("large.txt", 16)
	if err != nil || !large.Truncated || large.SHA256 != "" {
		t.Fatalf("oversized partial read advertised a version: %#v, %v", large, err)
	}
	window, err := view.ReadFileAt("large.txt", 1<<20, 16)
	if err != nil || window.Truncated || string(window.Content) != "a" || window.SHA256 != "" {
		t.Fatalf("large file continuation = %#v, %v", window, err)
	}
}

func TestContainerViewEditRejectsAmplificationBeforeMutation(t *testing.T) {
	workspace := t.TempDir()
	view := ContainerView{DefaultWorkdir: workspace}
	original := strings.Repeat("a", 2048)
	if _, err := view.WriteFile("task.txt", []byte(original)); err != nil {
		t.Fatal(err)
	}
	if _, err := view.EditFile("task.txt", "a", strings.Repeat("b", 1024), "", true, 4096); !errors.Is(err, ErrEditTooLarge) {
		t.Fatalf("amplifying edit = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "task.txt"))
	if err != nil || string(data) != original {
		t.Fatalf("rejected edit changed file: %q, %v", data, err)
	}
}

func TestContainerViewExactEditRejectsAmbiguousStaleAndNoOpRequests(t *testing.T) {
	view := ContainerView{DefaultWorkdir: t.TempDir()}
	if _, err := view.WriteFile("note.txt", []byte("old old")); err != nil {
		t.Fatal(err)
	}
	read, err := view.ReadFile("note.txt", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.EditFile("note.txt", "old", "new", read.SHA256, false, 1024); !errors.Is(err, ErrAmbiguousEdit) || errors.Is(err, ErrMutationOutcomeUnknown) {
		t.Fatalf("ambiguous edit = %v", err)
	}
	if _, err := view.EditFile("note.txt", "old", "new", "stale-version", true, 1024); !errors.Is(err, ErrChanged) || errors.Is(err, ErrMutationOutcomeUnknown) {
		t.Fatalf("stale edit = %v", err)
	}
	if _, err := view.EditFile("note.txt", "old", "old", read.SHA256, true, 1024); err == nil {
		t.Fatal("no-op edit was accepted")
	}
	if _, err := view.EditFile("note.txt", "old", "new", read.SHA256, true, 1024); err != nil {
		t.Fatal(err)
	}
	got, err := view.ReadFile("note.txt", 1024)
	if err != nil || string(got.Content) != "new new" {
		t.Fatalf("exact edit = %q, %v", got.Content, err)
	}
}

func TestContainerViewEditMatchesNormalizedNewlinesAndRestoresCRLF(t *testing.T) {
	view := ContainerView{DefaultWorkdir: t.TempDir()}
	if _, err := view.WriteFile("crlf.txt", []byte("first\r\nsecond\r\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := view.EditFile("crlf.txt", "first\nsecond", "changed\nsecond", "", false, 1024); err != nil {
		t.Fatal(err)
	}
	read, err := view.ReadFile("crlf.txt", 1024)
	if err != nil || string(read.Content) != "changed\r\nsecond\r\n" {
		t.Fatalf("CRLF edit = %q, %v", read.Content, err)
	}
	if _, err := view.EditFile("crlf.txt", "changed\r\nsecond", "changed\nsecond", "", false, 1024); err == nil {
		t.Fatal("LF-equivalent no-op edit was accepted")
	}
}

func TestLiteralEditSamplesLineEndingsByUTF16Units(t *testing.T) {
	prefix := strings.Repeat("🙂", 1500)
	original := prefix + strings.Repeat("\r\n", 50) + strings.Repeat("\n", 10) + "old"
	result, err := applyBoundedLiteralEdit([]byte(original), "old", "new", false, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := prefix + strings.Repeat("\r\n", 60) + "new"
	if string(result) != want {
		t.Fatal("Unicode prefix hid the CRLF-majority sample or changed unrelated content")
	}
	if got := firstUTF16Units("🙂a🙂b", 3); got != "🙂a" {
		t.Fatalf("UTF-16 sample = %q, want one emoji and one ASCII character", got)
	}
}

func TestContainerViewEditRejectsBinaryAndCRLFExpansion(t *testing.T) {
	view := ContainerView{DefaultWorkdir: t.TempDir()}
	for _, current := range []string{"a\x00b", "a\xffb"} {
		if _, err := view.WriteFile("binary.txt", []byte(current)); err != nil {
			t.Fatal(err)
		}
		if _, err := view.EditFile("binary.txt", "a", "c", "", false, 1024); !errors.Is(err, ErrEditNotText) {
			t.Fatalf("binary edit error = %v", err)
		}
	}
	if _, err := view.WriteFile("crlf.txt", []byte("a\r\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := view.EditFile("crlf.txt", "a", "long\nline", "", false, 10); !errors.Is(err, ErrEditTooLarge) {
		t.Fatalf("CRLF expansion was not bounded: %v", err)
	}
	read, err := view.ReadFile("crlf.txt", 1024)
	if err != nil || string(read.Content) != "a\r\n" {
		t.Fatalf("rejected edit changed CRLF file: %q, %v", read.Content, err)
	}
}

func TestContainerViewReplacesViaSameDirectoryWithoutTruncatingOldOpenFile(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "draft.txt")
	if err := os.WriteFile(path, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	view := ContainerView{DefaultWorkdir: workspace}
	if _, err := view.EditFile("draft.txt", "original", "replacement", "", false, 1024); err != nil {
		t.Fatal(err)
	}
	oldBytes, err := io.ReadAll(old)
	if err != nil || string(oldBytes) != "original" {
		t.Fatalf("old open file was truncated: %q, %v", oldBytes, err)
	}
	newBytes, err := os.ReadFile(path)
	if err != nil || string(newBytes) != "replacement" {
		t.Fatalf("replacement file = %q, %v", newBytes, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("replacement mode = %v, %v", info.Mode(), err)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil || len(entries) != 1 || entries[0].Name() != "draft.txt" {
		t.Fatalf("temporary edit files remained: %v, %v", entries, err)
	}
}

func TestContainerReplacementStagesPrivatelyOnTargetFilesystem(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "public.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	observedStaging := false
	err := replaceContainerFileWithRename(path, []byte("complete replacement"), 0o644, func(temp, target string) error {
		stagingDir := filepath.Dir(temp)
		if filepath.Dir(stagingDir) != workspace || target != path {
			t.Fatalf("replacement was not staged under the target parent: %q → %q", temp, target)
		}
		info, err := os.Stat(stagingDir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("staging directory is not private: %v, %v", info, err)
		}
		staged, err := os.ReadFile(temp)
		if err != nil || string(staged) != "complete replacement" {
			t.Fatalf("staged content is incomplete: %q, %v", staged, err)
		}
		original, err := os.ReadFile(target)
		if err != nil || string(original) != "old" {
			t.Fatalf("target changed before publication: %q, %v", original, err)
		}
		observedStaging = true
		return os.Rename(temp, target)
	})
	if err != nil || !observedStaging {
		t.Fatalf("private replacement = %v, observed=%t", err, observedStaging)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("published mode = %v, %v", info, err)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil || len(entries) != 1 || entries[0].Name() != "public.txt" {
		t.Fatalf("private staging directory was not removed: %v, %v", entries, err)
	}
}

func TestContainerFileRenameErrorHasUnknownOutcome(t *testing.T) {
	for _, committed := range []bool{false, true} {
		workspace := t.TempDir()
		path := filepath.Join(workspace, "draft.txt")
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		lostReply := errors.New("rename acknowledgement lost")
		err := replaceContainerFileWithRename(path, []byte("new"), 0o600, func(temp, target string) error {
			if committed {
				if err := os.Rename(temp, target); err != nil {
					return err
				}
			}
			return lostReply
		})
		if !errors.Is(err, ErrMutationOutcomeUnknown) || !errors.Is(err, lostReply) {
			t.Fatalf("rename error after committed=%t was not uncertain: %v", committed, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := "old"
		if committed {
			want = "new"
		}
		if string(got) != want {
			t.Fatalf("rename committed=%t left %q, want %q", committed, got, want)
		}
		entries, err := os.ReadDir(workspace)
		if err != nil || len(entries) != 1 || entries[0].Name() != "draft.txt" {
			t.Fatalf("rename failure left staging files: %v, %v", entries, err)
		}
	}
}

func TestContainerViewCanceledMutationDoesNotPublish(t *testing.T) {
	workspace := t.TempDir()
	view := ContainerView{DefaultWorkdir: workspace}
	if _, err := view.WriteFile("draft.txt", []byte("original")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := view.WriteFileContext(ctx, "draft.txt", []byte("replacement")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write = %v", err)
	}
	if _, err := view.EditFileContext(ctx, "draft.txt", "original", "replacement", "", false, 1024); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled edit = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "draft.txt")); err != nil || string(got) != "original" {
		t.Fatalf("canceled mutation changed target: %q, %v", got, err)
	}
}

func TestContainerFileCancellationAfterPublicationReportsCommittedResult(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "draft.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := replaceContainerFileWithContextAndRename(ctx, path, []byte("replacement"), 0o600, func(temp, target string) error {
		if err := os.Rename(temp, target); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if err != nil {
		t.Fatalf("committed write was reported canceled: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "replacement" {
		t.Fatalf("committed result = %q, %v", got, err)
	}
}

type cancelBeforePublicationContext struct {
	context.Context
	checks atomic.Int32
}

func (c *cancelBeforePublicationContext) Err() error {
	if c.checks.Add(1) > 1 {
		return context.Canceled
	}
	return nil
}

func TestContainerFileCancellationBeforePublicationKeepsOldFile(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "draft.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := &cancelBeforePublicationContext{Context: context.Background()}
	renamed := false
	err := replaceContainerFileWithContextAndRename(ctx, path, []byte("replacement"), 0o600, func(temp, target string) error {
		renamed = true
		return os.Rename(temp, target)
	})
	if !errors.Is(err, context.Canceled) || renamed {
		t.Fatalf("canceled staged write = %v, rename attempted=%t", err, renamed)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "original" {
		t.Fatalf("canceled staged write changed target: %q, %v", got, err)
	}
	if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 1 {
		t.Fatalf("canceled staged write left temp file: %v, %v", entries, err)
	}
}

func TestContainerViewSerializesGuardedEditsAcrossAliases(t *testing.T) {
	workspace := t.TempDir()
	view := ContainerView{DefaultWorkdir: workspace}
	if _, err := view.WriteFile("draft.txt", []byte("original")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(workspace, "alias.txt")
	if err := os.Symlink(filepath.Join(workspace, "draft.txt"), alias); err != nil {
		t.Fatal(err)
	}
	before, err := view.ReadFile("draft.txt", 1024)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, name := range []string{"draft.txt", "alias.txt"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			_, err := view.EditFile(name, "original", name, before.SHA256, false, 1024)
			results <- err
		}(name)
	}
	wg.Wait()
	close(results)
	succeeded, stale := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrChanged):
			stale++
		default:
			t.Fatalf("unexpected parallel edit result: %v", err)
		}
	}
	if succeeded != 1 || stale != 1 {
		t.Fatalf("parallel guarded edits: succeeded=%d stale=%d", succeeded, stale)
	}
}

func TestContainerViewRejectsSpecialFilesWithoutBlockingAgent(t *testing.T) {
	workspace := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(workspace, "task-pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	view := ContainerView{DefaultWorkdir: workspace}
	finished := make(chan error, 1)
	go func() {
		if _, err := view.ReadFile("task-pipe", 128); err == nil {
			finished <- errors.New("readfile accepted a FIFO")
			return
		}
		if _, err := view.WriteFile("task-pipe", []byte("must not write")); err == nil {
			finished <- errors.New("writefile accepted a FIFO")
			return
		}
		if _, err := view.EditFile("task-pipe", "old", "new", "", false, 128); err == nil {
			finished <- errors.New("editfile accepted a FIFO")
			return
		}
		if _, err := view.List("task-pipe", 10); err == nil {
			finished <- errors.New("listfiles accepted a FIFO")
			return
		}
		finished <- nil
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("container file tool blocked on a FIFO")
	}
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) / 2, nil }

func TestWriteAllRejectsShortWriteWithoutFalseDigest(t *testing.T) {
	if err := writeAll(shortWriter{}, []byte("content")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write = %v", err)
	}
}
