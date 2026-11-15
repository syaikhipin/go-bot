package durablefs

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEnsureDirectorySyncsEachNewParentInOrder(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "sessions", "project", "session")
	var synced []string
	err := ensureDirectory(target, os.Mkdir, func(parent string) error {
		synced = append(synced, parent)
		return SyncDirectory(parent)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Dir(root), root, filepath.Join(root, "sessions"), filepath.Join(root, "sessions", "project")}
	if !reflect.DeepEqual(synced, want) {
		t.Fatalf("new directory parent fsync order = %q, want %q", synced, want)
	}
	if err := EnsureDirectory(target); err != nil {
		t.Fatalf("reopening durable directory: %v", err)
	}
}

func TestEnsureDirectoryStopsBeforePublicationOnParentSyncFailure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "schedule", "tasks")
	failed := errors.New("directory fsync failed")
	err := ensureDirectory(target, os.Mkdir, func(parent string) error {
		if parent == root {
			return failed
		}
		return SyncDirectory(parent)
	})
	if !errors.Is(err, failed) {
		t.Fatalf("missing parent sync failure = %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created deeper directory after parent sync failure: %v", err)
	}
	var synced []string
	if err := ensureDirectory(target, os.Mkdir, func(parent string) error {
		synced = append(synced, parent)
		return SyncDirectory(parent)
	}); err != nil {
		t.Fatalf("retrying after parent fsync failure: %v", err)
	}
	if !reflect.DeepEqual(synced, []string{root, filepath.Join(root, "schedule")}) {
		t.Fatalf("retry skipped the previously failed parent fsync: %q", synced)
	}
}
