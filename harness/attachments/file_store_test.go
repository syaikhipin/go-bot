package attachments

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeFileStoreVerbatimReferenceSurvivesReopen(t *testing.T) {
	workspace := t.TempDir()
	store := FileStore{Root: workspace}
	data := []byte{0, 1, 2, 255, 0}
	ref, err := store.SaveFile(context.Background(), bytes.NewReader(data), `C:\private\notes.txt`)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Name != "notes.txt" || ref.Bytes != int64(len(data)) || !strings.HasPrefix(ref.ID, "sha256:") {
		t.Fatalf("invalid native file ref: %+v", ref)
	}
	path := (FileStore{Root: workspace}).Path(ref) // a recreated worker
	if path == "" {
		t.Fatal("durable native file reference has no path after reopen")
	}
	read, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(read, data) {
		t.Fatalf("native attachment changed bytes: %x, %v", read, err)
	}
	duplicate, err := store.SaveFile(context.Background(), bytes.NewReader(data), `notes.txt`)
	if err != nil || duplicate != ref {
		t.Fatalf("deduplicated native file = %+v, %v", duplicate, err)
	}
	other, err := store.SaveFile(context.Background(), bytes.NewReader(data), `other.txt`)
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.objectPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, firstErr := os.Stat(path)
	otherInfo, otherErr := os.Stat(store.Path(other))
	objectInfo, objectErr := os.Stat(object)
	if firstErr != nil || otherErr != nil || objectErr != nil || !os.SameFile(firstInfo, objectInfo) || !os.SameFile(otherInfo, objectInfo) {
		t.Fatalf("equal bytes did not share one object: first=%v other=%v object=%v", firstErr, otherErr, objectErr)
	}
	corruptAlias := filepath.Join(filepath.Dir(path), "replacement.tmp")
	if err := os.WriteFile(corruptAlias, []byte("mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(corruptAlias, path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveFile(context.Background(), bytes.NewReader(data), `notes.txt`); err == nil {
		t.Fatal("accepted a corrupted existing attachment object")
	}
}

func TestNativeFileStoreRejectsCorruptCanonicalObject(t *testing.T) {
	store := FileStore{Root: t.TempDir()}
	data := []byte("original")
	ref, err := store.SaveFile(context.Background(), bytes.NewReader(data), "first.txt")
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.objectPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	corruptObject := filepath.Join(filepath.Dir(object), "replacement.tmp")
	if err := os.WriteFile(corruptObject, []byte("changed!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(corruptObject, object); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveFile(context.Background(), bytes.NewReader(data), "second.txt"); err == nil {
		t.Fatal("created an alias for a corrupted canonical object")
	}
}

func TestNativeFileStoreSanitizesDisplayName(t *testing.T) {
	for input, want := range map[string]string{
		`C:\secret\report?.txt`: "report_.txt",
		`/private/CON.txt`:      "_CON.txt",
		`../`:                   "file",
		`hidden\dir\memo.md`:    "memo.md",
	} {
		if got := fileLeafName(input); got != want {
			t.Errorf("fileLeafName(%q) = %q, want %q", input, got, want)
		}
	}
	store := FileStore{Root: t.TempDir()}
	if path := store.Path(FileRef{ID: "sha256:../../etc", Name: "x", Bytes: 1}); path != "" {
		t.Fatalf("invalid ref resolved to %q", path)
	}
}

func TestNativeFileStorePublishesCompleteCopyAfterInterruptedStage(t *testing.T) {
	store := FileStore{Root: t.TempDir()}
	data := []byte("complete attachment")
	ref, err := store.SaveFile(context.Background(), bytes.NewReader(data), "first.txt")
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(store.Path(ref))
	stale := filepath.Join(directory, ".attachment-interrupted")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	const concurrent = 8
	results := make(chan error, concurrent)
	for range concurrent {
		go func() {
			_, err := store.SaveFile(context.Background(), bytes.NewReader(data), "second.txt")
			results <- err
		}()
	}
	for range concurrent {
		if err := <-results; err != nil {
			t.Fatalf("concurrent publish of same content: %v", err)
		}
	}
	second := FileRef{ID: ref.ID, Name: "second.txt", Bytes: ref.Bytes}
	if got, err := os.ReadFile(store.Path(second)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("published attachment = %q, %v", got, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".attachment-") && entry.Name() != filepath.Base(stale) {
			t.Fatalf("upload left its staging file behind: %s", entry.Name())
		}
	}
}
