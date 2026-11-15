package agentfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestObservedContainerViewRequiresReadBeforeExistingMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	view := NewObservedContainerView(ContainerView{DefaultWorkdir: root})
	if _, err := view.WriteFile("note.txt", []byte("blind")); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("blind overwrite = %v", err)
	}
	if _, err := view.EditFile("note.txt", "first", "blind", "", false, 1<<20); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("unobserved edit = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "first" {
		t.Fatalf("rejected mutations changed file: %q, %v", got, err)
	}
	if _, err := view.ReadFile("note.txt", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := view.EditFileContext(context.Background(), path, "first", "second", "", false, 1<<20); err != nil {
		t.Fatalf("read via relative path, edit via absolute alias: %v", err)
	}
	if _, err := view.WriteFile("note.txt", []byte("third")); err != nil {
		t.Fatalf("successful edit did not update observed version: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "third" {
		t.Fatalf("mutated file = %q, %v", got, err)
	}
	// Observation is process-local, not reconstructed from the Session log.
	cold := NewObservedContainerView(ContainerView{DefaultWorkdir: root})
	if _, err := cold.WriteFile("note.txt", []byte("cold")); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("cold Session inherited file observation: %v", err)
	}
}

func TestObservedContainerViewRejectsStaleVersionAndAllowsFreshRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	view := NewObservedContainerView(ContainerView{DefaultWorkdir: root})
	if _, err := view.ReadFile("note.txt", 8); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("external change"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := view.WriteFile("note.txt", []byte("stale")); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale overwrite = %v", err)
	}
	if _, err := view.EditFile("note.txt", "external", "stale", "", false, 1<<20); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale edit = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "external change" {
		t.Fatalf("stale mutation changed file: %q, %v", got, err)
	}
	if _, err := view.ReadFile("note.txt", 8); err != nil {
		t.Fatal(err)
	}
	if _, err := view.WriteFile("note.txt", []byte("fresh")); err != nil {
		t.Fatalf("fresh overwrite = %v", err)
	}
}

func TestObservedContainerViewCreateOnlyAndCanonicalSymlink(t *testing.T) {
	root := t.TempDir()
	view := NewObservedContainerView(ContainerView{DefaultWorkdir: root})
	if _, err := view.ReadFile("missing.txt", 16); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing read = %v", err)
	}
	if _, err := view.EditFile("missing.txt", "old", "new", "", false, 1<<20); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed absent edit = %v", err)
	}
	if _, err := view.WriteFile("missing.txt", []byte("created")); err != nil {
		t.Fatalf("create after absent read = %v", err)
	}
	if err := os.Symlink("missing.txt", filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := view.ReadFile("alias.txt", 16); err != nil {
		t.Fatal(err)
	}
	if _, err := view.WriteFile("missing.txt", []byte("via canonical target")); err != nil {
		t.Fatalf("alias observation did not apply to canonical target: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "alias.txt")); err != nil || string(got) != "via canonical target" {
		t.Fatalf("alias target = %q, %v", got, err)
	}
}

func TestObservedContainerViewLargePartialReadCarriesVersion(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(strings.Repeat("x", 1<<20+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	view := NewObservedContainerView(ContainerView{DefaultWorkdir: root})
	read, err := view.ReadFileAt("large.txt", 0, 16)
	if err != nil || !read.Truncated || read.SHA256 != "" {
		t.Fatalf("large read = %+v, %v", read, err)
	}
	if err := view.RecordReadObservation(read); err != nil {
		t.Fatal(err)
	}
	if _, err := view.WriteFile("large.txt", []byte("small")); err != nil {
		t.Fatalf("bounded read did not permit guarded overwrite: %v", err)
	}
}

func TestCreateContainerFileIfAbsentNeverClobbersAnotherFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "already.txt")
	if err := os.WriteFile(path, []byte("owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createContainerFileIfAbsent(context.Background(), path, []byte("intruder")); !errors.Is(err, ErrNotObserved) {
		t.Fatalf("guarded create collision = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "owner" {
		t.Fatalf("guarded create clobbered target: %q, %v", got, err)
	}
}

func TestContainerCreateStagesPrivatelyBeforeHardLinkPublication(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "new.txt")
	observedStaging := false
	err := createContainerFileIfAbsentWithLink(context.Background(), target, []byte("complete content"), func(temp, destination string) error {
		stagingDir := filepath.Dir(temp)
		if filepath.Dir(stagingDir) != root || destination != target {
			t.Fatalf("new file was staged outside its target parent: %q → %q", temp, destination)
		}
		info, err := os.Stat(stagingDir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("new file staging directory is not private: %v, %v", info, err)
		}
		staged, err := os.ReadFile(temp)
		if err != nil || string(staged) != "complete content" {
			t.Fatalf("staged new file is incomplete: %q, %v", staged, err)
		}
		if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("new target appeared before publication: %v", err)
		}
		observedStaging = true
		return os.Link(temp, destination)
	})
	if err != nil || !observedStaging {
		t.Fatalf("private new-file publication = %v, observed=%t", err, observedStaging)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "complete content" {
		t.Fatalf("published new file = %q, %v", content, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "new.txt" {
		t.Fatalf("new-file staging directory was not removed: %v, %v", entries, err)
	}
}

func TestContainerCreateLostHardLinkReplyHasUnknownOutcome(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "new.txt")
	lostReply := errors.New("hard-link acknowledgement lost")
	err := createContainerFileIfAbsentWithLink(context.Background(), target, []byte("complete content"), func(temp, destination string) error {
		if err := os.Link(temp, destination); err != nil {
			return err
		}
		return lostReply
	})
	if !errors.Is(err, ErrMutationOutcomeUnknown) || !errors.Is(err, lostReply) {
		t.Fatalf("lost hard-link reply = %v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "complete content" {
		t.Fatalf("committed create disappeared after lost reply: %q, %v", content, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "new.txt" {
		t.Fatalf("lost-reply create left staging directory: %v, %v", entries, err)
	}
}
