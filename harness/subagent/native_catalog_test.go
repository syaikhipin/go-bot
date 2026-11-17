package subagent

import (
	"context"
	"testing"

	"go-bot/harness/session"
)

type nativeCatalogJournal struct{ events []sessionformat.Event }

func (j *nativeCatalogJournal) Append(_ context.Context, events []sessionformat.Event) error {
	j.events = append(j.events, events...)
	return nil
}

func (*nativeCatalogJournal) Close() error { return nil }

func TestRecordNativeChildWritesParentOwnedV4Catalog(t *testing.T) {
	ctx := context.Background()
	journal := &nativeCatalogJournal{}
	parent, err := sessionformat.NewLiveSession(sessionformat.Header{
		Type: "session", Version: 4, ID: "root", CreatedAt: 1, CWD: "/workspace",
	}, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	child := sessionformat.Header{
		Type: "session", Version: 4, ID: "child", CreatedAt: 2, CWD: "/workspace",
		ParentSession: "root", Origin: "subagent", DelegationDepth: 1,
	}
	entry, err := RecordNativeChild(ctx, parent, child, "Research")
	if err != nil || entry.ChildID != "child" || entry.Mode != "continuable" {
		t.Fatalf("catalog entry = %+v, %v", entry, err)
	}
	if len(journal.events) != 1 || journal.events[0].Type != "subagent/catalog" {
		t.Fatalf("parent catalog events = %+v", journal.events)
	}
	children, err := sessionformat.FoldSubagentCatalog(parent.DurableEvents(), 0)
	if err != nil || len(children) != 1 || children[0].ChildID != "child" {
		t.Fatalf("parent catalog = %+v, %v", children, err)
	}
	if _, err := RecordNativeChild(ctx, parent, child, "Research"); err == nil {
		t.Fatal("duplicate native child was published twice")
	}
	child.ParentSession = "other"
	if _, err := RecordNativeChild(ctx, parent, child, "Research"); err == nil {
		t.Fatal("foreign native child was cataloged")
	}
}
