package durablefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SyncDirectory makes a newly published child entry durable at its parent.
// The target filesystem must support directory fsync; Host startup probes
// that capability before it accepts Sessions.
func SyncDirectory(directory string) error {
	f, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// EnsureDirectory creates missing path components and syncs each new entry's
// parent before a journal, task table or attachment is published below it.
// Syncing only the innermost directory would not make its own path durable.
func EnsureDirectory(directory string) error {
	return ensureDirectory(directory, os.Mkdir, SyncDirectory)
}

func ensureDirectory(directory string, mkdir func(string, os.FileMode) error, syncParent func(string) error) error {
	if !filepath.IsAbs(directory) {
		return errors.New("durable directory path must be absolute")
	}
	var missing []string
	var existing string
	for path := directory; ; path = filepath.Dir(path) {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("durable path %q is not a directory", path)
			}
			existing = path
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, path)
		if path == filepath.Dir(path) {
			return errors.New("durable directory has no existing parent")
		}
	}
	// A previous attempt may have created this component but failed to sync
	// its entry in the parent. Re-syncing the deepest existing entry makes a
	// retry safe before extending the path or publishing a file below it.
	if existing != filepath.Dir(existing) {
		if err := syncParent(filepath.Dir(existing)); err != nil {
			return fmt.Errorf("sync durable directory parent %q: %w", filepath.Dir(existing), err)
		}
	}
	for index := len(missing) - 1; index >= 0; index-- {
		path := missing[index]
		if err := mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("durable directory %q is unavailable: %w", path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("durable path %q is not a directory", path)
		}
		if err := syncParent(filepath.Dir(path)); err != nil {
			return fmt.Errorf("sync durable directory parent %q: %w", filepath.Dir(path), err)
		}
	}
	return nil
}
