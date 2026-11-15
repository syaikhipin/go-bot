//go:build linux

package agentfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestContainerViewRejectsWorkerAndKernelPseudoFiles(t *testing.T) {
	workspace := t.TempDir()
	view := ContainerView{DefaultWorkdir: workspace}
	for _, path := range []string{"/proc/self/environ", "/proc/self/mem", "/proc/self/fd/1", "/sys/kernel"} {
		if _, err := view.ReadFile(path, 128); err == nil {
			t.Errorf("read of %q was allowed", path)
		}
	}
	if _, err := view.WriteFile("/proc/self/fd/1", []byte("must-not-reach-private-output")); err == nil {
		t.Fatal("write to worker output descriptor was allowed")
	}
	if _, err := view.EditFile("/proc/self/mem", "old", "new", "", false, 128); err == nil {
		t.Fatal("edit of worker memory was allowed")
	}
	if _, err := view.Search(context.Background(), SearchOptions{Path: "/proc", Pattern: "test", Kind: "name", MaxMatches: 1}); err == nil {
		t.Fatal("search through procfs was allowed")
	}
	link := filepath.Join(workspace, "private-output")
	if err := os.Symlink("/proc/self/fd/1", link); err != nil {
		t.Fatal(err)
	}
	if _, err := view.WriteFile(link, []byte("must-not-reach-private-output")); err == nil {
		t.Fatal("write through procfs magic-link alias was allowed")
	}
}

func TestContainerViewKeepsOrdinaryNativeFilesAndSymlinksWritable(t *testing.T) {
	workspace := t.TempDir()
	view := ContainerView{DefaultWorkdir: workspace}
	path := filepath.Join(workspace, "task.txt")
	if _, err := view.WriteFile(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(workspace, "alias.txt")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := view.WriteFile(alias, []byte("second")); err != nil {
		t.Fatal(err)
	}
	read, err := view.ReadFile(path, 128)
	if err != nil || string(read.Content) != "second" {
		t.Fatalf("native file read = %q, %v", read.Content, err)
	}
	if _, err := view.EditFile(alias, "second", "third", read.SHA256, false, 128); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "third" {
		t.Fatalf("native file content = %q, %v", data, err)
	}
}

func TestContainerWriteRejectsMagicLinkParentBeforeCreatingDirectory(t *testing.T) {
	workspace := t.TempDir()
	view := ContainerView{DefaultWorkdir: workspace}
	dir, err := os.Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	path := fmt.Sprintf("/proc/self/fd/%d/should-not-exist/file.txt", dir.Fd())
	if _, err := view.WriteFile(path, []byte("no side effect")); err == nil {
		t.Fatal("write through worker descriptor was accepted")
	}
	if _, err := os.Stat(filepath.Join(workspace, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("rejected write created a directory through procfs: %v", err)
	}
}
