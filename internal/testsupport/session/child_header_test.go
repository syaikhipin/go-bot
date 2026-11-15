package session

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestChildHeaderRequiresImmutableLineageAndSharedWorkspace(t *testing.T) {
	parent := uuid.NewString()
	child := appendStoreHeader()
	child.SessionID = uuid.NewString()
	child.ParentSessionID = parent
	child.WorkspaceOwnerID = parent
	child.DelegationDepth = 1
	if err := validateHeader(child); err != nil {
		t.Fatal(err)
	}
	if got := child.EffectiveWorkspaceOwnerID(); got != parent {
		t.Fatalf("child workspace owner = %s", got)
	}
	grandchild := child
	grandchild.SessionID = uuid.NewString()
	grandchild.ParentSessionID = child.SessionID
	grandchild.DelegationDepth = 2
	if err := validateHeader(grandchild); err != nil {
		t.Fatal(err)
	}
	if got := grandchild.EffectiveWorkspaceOwnerID(); got != parent {
		t.Fatalf("grandchild workspace owner = %s", got)
	}
	root := appendStoreHeader()
	if err := validateHeader(root); err != nil || root.EffectiveWorkspaceOwnerID() != root.SessionID {
		t.Fatalf("root header = %#v, %v", root, err)
	}
	for _, malformed := range []struct {
		name   string
		change func(*Header)
	}{
		{"missing workspace owner", func(h *Header) { h.WorkspaceOwnerID = "" }},
		{"wrong first-level workspace", func(h *Header) { h.WorkspaceOwnerID = uuid.NewString() }},
		{"self parent", func(h *Header) { h.ParentSessionID = h.SessionID }},
		{"missing depth", func(h *Header) { h.DelegationDepth = 0 }},
		{"bad parent ID", func(h *Header) { h.ParentSessionID = "not-a-uuid" }},
		{"bad workspace ID", func(h *Header) { h.WorkspaceOwnerID = "not-a-uuid" }},
	} {
		t.Run(malformed.name, func(t *testing.T) {
			probe := child
			malformed.change(&probe)
			if err := validateHeader(probe); err == nil {
				t.Fatalf("malformed child header accepted: %#v", probe)
			}
		})
	}
	badRoot := root
	badRoot.WorkspaceOwnerID = uuid.NewString()
	if err := validateHeader(badRoot); err == nil || !strings.Contains(err.Error(), "top-level") {
		t.Fatalf("root was allowed to masquerade as a child: %v", err)
	}
}
