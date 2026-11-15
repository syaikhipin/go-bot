package attachments

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go-harness/internal/durablefs"
)

// LocalObjectAPI persists immutable image bytes in the current Host's
// DSH home, whether its data is local or NAS-mounted.
// Store verifies the digest after every write and read.
type LocalObjectAPI struct{ Root string }

// LocalImageObjectPath resolves a durable image ID to the normalized object
// visible in the current Host filesystem. The path is derived from the ID,
// never from the original filename or an unlogged readimage source path.
func LocalImageObjectPath(root, id string) (string, error) {
	key, err := (Store{}).key(id)
	if err != nil {
		return "", err
	}
	return (LocalObjectAPI{Root: root}).objectPath(key)
}

func (a LocalObjectAPI) objectPath(key string) (string, error) {
	if !filepath.IsAbs(a.Root) || key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || path.Clean(key) != key || strings.HasPrefix(key, "../") {
		return "", errors.New("invalid local attachment object path")
	}
	return filepath.Join(a.Root, filepath.FromSlash(key)), nil
}

// HostPath exposes the normalized object location to a Host-local tool world.
func (a LocalObjectAPI) HostPath(key string) (string, error) {
	return a.objectPath(key)
}

func (a LocalObjectAPI) PutIfAbsent(ctx context.Context, key, _ string, data []byte) error {
	filename, err := a.objectPath(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := durablefs.EnsureDirectory(filepath.Dir(filename)); err != nil {
		return err
	}
	if _, err := os.Lstat(filename); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(filename), ".image-attachment-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Chmod(0o400)
	}
	syncErr := file.Sync()
	if err := errors.Join(writeErr, syncErr, file.Close()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Linking a completed same-directory staging file publishes the object
	// atomically without replacing another writer's immutable copy.
	if err := os.Link(file.Name(), filename); errors.Is(err, os.ErrExist) {
		return nil
	} else if err != nil {
		return err
	}
	return durablefs.SyncDirectory(filepath.Dir(filename))
}

func (a LocalObjectAPI) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	filename, err := a.objectPath(key)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return openAttachment(filename)
}
