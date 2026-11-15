package agentfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainerSearchFindsLineAndFilenameWithoutFollowingSymlink(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", "notes.txt"), []byte("first line\nneedle present\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(base, "secret.txt")
	if err := os.WriteFile(secret, []byte("needle secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(workspace, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested", filepath.Join(workspace, "loop")); err != nil {
		t.Fatal(err)
	}
	view := ContainerView{DefaultWorkdir: workspace}
	result, err := view.Search(context.Background(), SearchOptions{Pattern: "needle", MaxMatches: 10})
	if err != nil || len(result.Matches) != 1 || result.Matches[0].Path != filepath.Join(workspace, "nested", "notes.txt") || result.Matches[0].Line != 2 || strings.Contains(result.Matches[0].Text, "secret") {
		t.Fatalf("container search = %#v, %v", result, err)
	}
	names, err := view.Search(context.Background(), SearchOptions{Pattern: `notes\.txt$`, Kind: "name", MaxMatches: 10})
	if err != nil || len(names.Matches) != 1 || names.Matches[0].Path != filepath.Join(workspace, "nested", "notes.txt") {
		t.Fatalf("name search = %#v, %v", names, err)
	}
}

func TestContainerSearchUsesNativeFilesystemAndBoundsMatches(t *testing.T) {
	workspace := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "outside.txt"), []byte("needle outside\nneedle again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	view := ContainerView{DefaultWorkdir: workspace}
	result, err := view.Search(context.Background(), SearchOptions{Path: other, Pattern: "needle", MaxMatches: 1})
	if err != nil || len(result.Matches) != 1 || !result.Truncated || result.Matches[0].Path != filepath.Join(other, "outside.txt") {
		t.Fatalf("container search = %#v, %v", result, err)
	}
	if _, err := view.Search(context.Background(), SearchOptions{Path: other, Pattern: "(", MaxMatches: 1}); err == nil {
		t.Fatal("invalid regex was accepted")
	}
}

func TestSearchMarksOversizedFileWithoutReadingIt(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "large.txt"), []byte(strings.Repeat("a", searchMaxFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	view := ContainerView{DefaultWorkdir: workspace}
	result, err := view.Search(context.Background(), SearchOptions{Pattern: "a", MaxMatches: 10})
	if err != nil || len(result.Matches) != 0 || !result.Truncated || result.Reason != "large file skipped" {
		t.Fatalf("oversized file search = %#v, %v", result, err)
	}
}
