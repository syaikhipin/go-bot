package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileSessionListingSkipsWorkspaceOnlyAndPaginates(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := FileStore{Root: root, Claim: noOpClaim}
	for _, id := range []string{"a", "b", "c"} {
		header := Header{SchemaVersion: SchemaVersion, SessionID: id, UserID: "alice", Mode: "container", Workspace: "/workspace", CreatedAt: time.Now().UTC()}
		journal, err := store.Create(ctx, header)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "users", "alice", "sessions", "z", "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(root, "users", "alice", "sessions", "zz")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, JournalFilename), []byte("not JSON\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := store.ListSessions(ctx, "alice", "", 2)
	if err != nil || len(first.Headers) != 2 || first.NextCursor != "b" {
		t.Fatalf("first local Session page = %#v, %v", first, err)
	}
	second, err := store.ListSessions(ctx, "alice", first.NextCursor, 2)
	if err != nil || len(second.Headers) != 1 || second.Headers[0].SessionID != "c" || second.NextCursor != "" {
		t.Fatalf("second local Session page = %#v, %v", second, err)
	}
}

func TestHeaderOnlyReadIsBoundedButAcceptsLongValidMetadata(t *testing.T) {
	root := t.TempDir()
	store := FileStore{Root: root, Claim: noOpClaim}
	header := Header{SchemaVersion: SchemaVersion, SessionID: "long", UserID: "alice", Mode: "container", Workspace: "/workspace/" + strings.Repeat("v", 9000), CreatedAt: time.Now().UTC()}
	journal, err := store.Create(context.Background(), header)
	if err != nil {
		t.Fatal(err)
	}
	_ = journal.Close()
	if selected, err := store.ReadHeader("alice", "long"); err != nil || selected.Workspace != header.Workspace {
		t.Fatalf("long valid header = %#v, %v", selected, err)
	}
	directory, err := SessionDirectory(root, "alice", "oversized")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, JournalFilename), []byte(strings.Repeat("x", maxHeaderLineBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadHeader("alice", "oversized"); err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("oversized header rejection = %v", err)
	}
}
