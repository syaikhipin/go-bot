package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go-bot/harness/session"
)

func TestHostListsOnlyActivatedTopLevelSessionsWithPages(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ids := []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}
	for i, id := range ids {
		workspace := filepath.Join(root, "workspaces", id)
		if err := os.MkdirAll(workspace, 0700); err != nil {
			t.Fatal(err)
		}
		store := sessionformat.FileStore{
			Root: filepath.Join(root, ".dsh", "sessions"), ProjectCWD: workspace,
			Claim: func(context.Context, string) (func(), error) { return func() {}, nil },
		}
		journal, err := store.Create(ctx, sessionformat.Header{Type: "session", Version: sessionformat.Version, ID: id, CreatedAt: 1700000000000 - int64(i), CWD: workspace})
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// A child log is stored beside its parent, but cannot become a client
	// Session merely because it has its own valid v4 header and UUID.
	childID := "00000000-0000-4000-8000-000000000004"
	childStore := sessionformat.FileStore{
		Root: filepath.Join(root, ".dsh", "sessions"), ProjectCWD: filepath.Join(root, "workspaces", ids[0]),
		RootSessionID: ids[0], Claim: func(context.Context, string) (func(), error) { return func() {}, nil },
	}
	child, err := childStore.Create(ctx, sessionformat.Header{
		Type: "session", Version: sessionformat.Version, ID: childID, CreatedAt: 1700000000001,
		CWD: childStore.ProjectCWD, ParentSession: ids[0], Origin: "subagent", DelegationDepth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	unactivated := "00000000-0000-4000-8000-000000000003"
	if err := os.MkdirAll(filepath.Join(root, "workspaces", unactivated), 0700); err != nil {
		t.Fatal(err)
	}
	page, err := listHostSessions(ctx, root, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || page.Sessions[0].ID != ids[0] || page.NextCursor != ids[0] {
		t.Fatalf("first page = %+v", page)
	}
	page, err = listHostSessions(ctx, root, page.NextCursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || page.Sessions[0].ID != ids[1] || page.NextCursor != "" {
		t.Fatalf("second page = %+v", page)
	}
	// Deleting or renaming ordinary workspace files must not erase the
	// Session from the history index; its v4 log remains authoritative.
	if err := os.Remove(filepath.Join(root, "workspaces", ids[0])); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "workspaces", ids[0])); !os.IsNotExist(err) {
		t.Fatalf("first workspace unexpectedly exists: %v", err)
	}
	page, err = listHostSessions(ctx, root, "", 100)
	if err != nil || len(page.Sessions) != 2 || page.Sessions[0].ID != ids[0] || page.Sessions[1].ID != ids[1] {
		t.Fatalf("history after workspace removal = %+v, %v", page, err)
	}
	page, err = latestHostSession(ctx, root)
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != ids[0] || page.NextCursor != "" {
		t.Fatalf("latest top-level session by creation time = %+v, %v", page, err)
	}
	parentStore := sessionformat.FileStore{Root: childStore.Root, ProjectCWD: childStore.ProjectCWD}
	if err := archiveChat(ctx, parentStore, ids[0]); err != nil {
		t.Fatal(err)
	}
	page, err = latestHostSession(ctx, root)
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != ids[1] {
		t.Fatalf("latest session after archive = %+v, %v", page, err)
	}
}
