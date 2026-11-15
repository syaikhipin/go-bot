package attachments

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalObjectAPIPreservesImmutableBytes(t *testing.T) {
	api := LocalObjectAPI{Root: t.TempDir()}
	ctx := context.Background()
	key := "attachments/v1/objects/ab/abcdef"
	if err := api.PutIfAbsent(ctx, key, "image/png", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := api.PutIfAbsent(ctx, key, "image/png", []byte("second")); err != nil {
		t.Fatal(err)
	}
	reader, err := api.Read(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "first" {
		t.Fatalf("immutable object = %q, %v", data, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../escape", "/absolute", "attachments/../escape", `attachments\escape`} {
		if err := api.PutIfAbsent(ctx, bad, "image/png", []byte("bad")); err == nil {
			t.Fatalf("unsafe attachment key %q accepted", bad)
		}
	}
	if _, err := api.Read(ctx, "attachments/missing"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("missing local attachment = %v", err)
	}
}

func TestLocalImageObjectPathUsesValidatedDurableIdentity(t *testing.T) {
	root := t.TempDir()
	id := "sha256:" + strings.Repeat("a", 64)
	got, err := LocalImageObjectPath(root, id)
	if err != nil || got != filepath.Join(root, "attachments", "v1", "objects", "aa", strings.Repeat("a", 64)) {
		t.Fatalf("normalized image path = %q, %v", got, err)
	}
	for _, invalid := range []string{"sha256:../escape", "sha256:" + strings.Repeat("A", 64), "other:" + strings.Repeat("a", 64)} {
		if _, err := LocalImageObjectPath(root, invalid); err == nil {
			t.Fatalf("invalid durable image ID %q resolved", invalid)
		}
	}
}

func TestStoreExposesReadOnlyNormalizedHostPath(t *testing.T) {
	root := t.TempDir()
	store := Store{API: LocalObjectAPI{Root: root}}
	ref, err := store.SaveImage(context.Background(), tinyImage(t), "image/png", "source.png", "/workspace/source.png")
	if err != nil {
		t.Fatal(err)
	}
	path := store.ImageHostPath(ref)
	want, err := LocalImageObjectPath(root, ref.ID)
	if err != nil || path != want || path == ref.ReadPath {
		t.Fatalf("Host image path = %q, want %q: %v", path, want, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("normalized object mode = %v, %v; want 0400", info, err)
	}
	if got := (Store{API: &memoryObjectAPI{}}).ImageHostPath(ref); got != "" {
		t.Fatalf("nonlocal object exposed a host path: %q", got)
	}
}

func TestLocalObjectAPIPublishesOnlyCompleteImageObjects(t *testing.T) {
	api := LocalObjectAPI{Root: t.TempDir()}
	key := "attachments/v1/objects/ab/abcdef"
	filename, err := api.objectPath(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(filepath.Dir(filename), ".image-attachment-interrupted")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	const concurrent = 8
	results := make(chan error, concurrent)
	for range concurrent {
		go func() { results <- api.PutIfAbsent(context.Background(), key, "image/png", []byte("complete")) }()
	}
	for range concurrent {
		if err := <-results; err != nil {
			t.Fatalf("concurrent image object publication: %v", err)
		}
	}
	data, err := os.ReadFile(filename)
	if err != nil || string(data) != "complete" {
		t.Fatalf("published image object = %q, %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Dir(filename))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".image-attachment-") && entry.Name() != filepath.Base(stale) {
			t.Fatalf("image upload left its staging file behind: %s", entry.Name())
		}
	}
}
