package executor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go-bot/harness/session"
	"go-bot/internal/testsupport/session"
)

func TestNativeFileStorageRestoresParentChildAndGrandchild(t *testing.T) {
	ctx := context.Background()
	claims := &session.ProcessClaims{}
	cwd := "/root/harness/workspaces/root"
	root := NativeFileStorage{
		Store:     sessionformat.FileStore{Root: filepath.Join(t.TempDir(), ".dsh", "sessions"), ProjectCWD: cwd, Claim: claims.Acquire},
		SessionID: "root",
	}
	header := sessionformat.Header{Type: "session", Version: sessionformat.Version, ID: "root", CreatedAt: time.Now().UnixMilli(), CWD: cwd}
	journal, err := root.CreateNative(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	childStorage, err := root.ChildNativeStorage("child")
	if err != nil {
		t.Fatal(err)
	}
	child := childStorage.(NativeFileStorage)
	childHeader := header
	childHeader.ID = "child"
	childHeader.ParentSession = "root"
	childHeader.Origin = "subagent"
	childHeader.DelegationDepth = 1
	childJournal, err := child.CreateNative(ctx, childHeader)
	if err != nil {
		t.Fatal(err)
	}
	if err := childJournal.Close(); err != nil {
		t.Fatal(err)
	}
	grandchildStorage, err := child.ChildNativeStorage("grandchild")
	if err != nil {
		t.Fatal(err)
	}
	grandchild := grandchildStorage.(NativeFileStorage)
	grandchildHeader := childHeader
	grandchildHeader.ID = "grandchild"
	grandchildHeader.ParentSession = "child"
	grandchildHeader.DelegationDepth = 2
	grandchildJournal, err := grandchild.CreateNative(ctx, grandchildHeader)
	if err != nil {
		t.Fatal(err)
	}
	if err := grandchildJournal.Close(); err != nil {
		t.Fatal(err)
	}
	for _, storage := range []NativeFileStorage{root, child, grandchild} {
		opened, artifact, err := storage.OpenNative(ctx)
		if err != nil || artifact.Header.ID != storage.SessionID {
			t.Fatalf("reopen %s = %+v, %v", storage.SessionID, artifact.Header, err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
