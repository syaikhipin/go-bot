package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureSessionWorkspaceCreatesOnlyRealPrivateWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace, err := EnsureSessionWorkspaceOnly(root, "alice", "s1")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(workspace)
	if err != nil || !info.IsDir() {
		t.Fatalf("Session path %s = %#v, %v", workspace, info, err)
	}
	if _, err := EnsureSessionWorkspaceOnly(root, "alice", "s1"); err != nil {
		t.Fatalf("existing real Session directories were rejected: %v", err)
	}
}

func TestEnsureSessionWorkspaceOnlyDoesNotCreateDeliveryDirectory(t *testing.T) {
	root := t.TempDir()
	workspace, err := EnsureSessionWorkspaceOnly(root, "alice", "container-1")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("workspace = %#v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(workspace), "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("container Session created a delivery directory: %v", err)
	}
}

func TestEnsureSessionWorkspaceRejectsSymlinkBeforeDescendantCreation(t *testing.T) {
	root := t.TempDir()
	victim := t.TempDir()
	parent := filepath.Join(root, "users", "alice", "sessions")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(parent, "s1")); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureSessionWorkspaceOnly(root, "alice", "s1"); err == nil {
		t.Fatal("symlinked Session identity was accepted")
	}
	if _, err := os.Stat(filepath.Join(victim, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("workspace was created outside Session tree: %v", err)
	}
	if err := os.Mkdir(filepath.Join(parent, "s2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(parent, "s2", "workspace")); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureSessionWorkspaceOnly(root, "alice", "s2"); err == nil {
		t.Fatal("symlinked workspace root was accepted")
	}
	if _, err := os.Stat(filepath.Join(parent, "s2", "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("artifact directory created after workspace rejection: %v", err)
	}
	alias := filepath.Join(t.TempDir(), "mount-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureSessionWorkspaceOnly(alias, "alice", "s3"); err == nil {
		t.Fatal("symlinked mount root was accepted")
	}
}

func TestOpenExistingSessionWorkspaceRejectsSymlinksAndDoesNotCreate(t *testing.T) {
	root := t.TempDir()
	if _, err := OpenExistingSessionWorkspace(root, "alice", "missing"); !os.IsNotExist(err) {
		t.Fatalf("missing historical workspace = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "users")); !os.IsNotExist(err) {
		t.Fatalf("historical read created a Session parent: %v", err)
	}
	workspace, err := EnsureSessionWorkspaceOnly(root, "alice", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "answer.txt"), []byte("own data"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenExistingSessionWorkspace(root, "alice", "s1")
	if err != nil {
		t.Fatal(err)
	}
	file, err := opened.Open("answer.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	_ = opened.Close()
	if err := os.Rename(workspace, workspace+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExistingSessionWorkspace(root, "alice", "s1"); err == nil {
		t.Fatal("symlinked historical workspace was opened")
	}
	other := filepath.Join(root, "users", "bob")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(root, "users", "charlie")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExistingSessionWorkspace(root, "charlie", "s1"); err == nil {
		t.Fatal("symlinked historical Session parent was opened")
	}
}
