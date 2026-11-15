package agentfs

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ContainerView exposes the container's native filesystem. The container
// mount namespace, not this provider, is the permission boundary. Image-local
// Skill copies are ordinary writable files in the private container layer;
// the OSS Skill source repository is never mounted here.
type ContainerView struct {
	DefaultWorkdir string
}

// File-tool mutations share a bounded set of in-process locks, keyed by the
// resolved target so ordinary symlink aliases serialize with their target.
// Shell processes are external writers and are not covered by these locks.
var containerMutationLocks [256]sync.Mutex

func containerMutationLock(path string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(path))
	return &containerMutationLocks[h.Sum32()%uint32(len(containerMutationLocks))]
}

type containerOpenKind uint8

const (
	containerRegular containerOpenKind = iota
	containerDirectory
	containerRegularOrDirectory
)

func (v ContainerView) path(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(v.DefaultWorkdir) {
		return "", errors.New("invalid container file path or default workdir")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(v.DefaultWorkdir, path)
	}
	return filepath.Clean(path), nil
}

func (v ContainerView) ReadFile(path string, maxBytes int64) (ReadResult, error) {
	return v.ReadFileAt(path, 0, maxBytes)
}

func (v ContainerView) ReadFileAt(path string, offset, maxBytes int64) (ReadResult, error) {
	if maxBytes <= 0 {
		return ReadResult{}, errors.New("positive read limit required")
	}
	path, err := v.path(path)
	if err != nil {
		return ReadResult{}, err
	}
	file, err := openContainerExisting(path, os.O_RDONLY, containerRegular)
	if err != nil {
		return ReadResult{}, err
	}
	defer file.Close()
	result, err := readBoundedFileAt(file, offset, maxBytes)
	if err != nil {
		return ReadResult{}, err
	}
	result.ResolvedPath = resolvedOpenFilePath(file, path)
	return result, nil
}

func (v ContainerView) List(path string, maxEntries int) ([]string, error) {
	if maxEntries <= 0 {
		return nil, errors.New("positive entry limit required")
	}
	path, err := v.path(path)
	if err != nil {
		return nil, err
	}
	dir, err := openContainerExisting(path, os.O_RDONLY, containerDirectory)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(maxEntries)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return names, err
}

func (v ContainerView) Search(ctx context.Context, options SearchOptions) (SearchResult, error) {
	path := options.Path
	if path == "" {
		path = "."
	}
	resolved, err := v.path(path)
	if err != nil {
		return SearchResult{}, err
	}
	return searchFiles(ctx, searchFileSystem{lstat: os.Lstat, open: func(path string) (*os.File, error) {
		return openContainerExisting(path, os.O_RDONLY, containerRegularOrDirectory)
	}}, resolved, resolved, options)
}

func (v ContainerView) WriteFile(path string, data []byte) (string, error) {
	return v.WriteFileContext(context.Background(), path, data)
}

func (v ContainerView) WriteFileContext(ctx context.Context, path string, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := v.path(path)
	if err != nil {
		return "", err
	}
	if err := ensureContainerParent(path); err != nil {
		return "", uncertainMutation(fmt.Errorf("create file parent: %w", err))
	}
	target, mode, err := containerMutationTarget(path)
	if err != nil {
		return "", err
	}
	lock := containerMutationLock(target)
	lock.Lock()
	defer lock.Unlock()
	// A different tool may have replaced this target while we waited.
	currentTarget, currentMode, err := containerMutationTarget(path)
	if err != nil {
		return "", err
	}
	if currentTarget != target {
		return "", ErrChanged
	}
	mode = currentMode
	if err := replaceContainerFileContext(ctx, target, data, mode); err != nil {
		return "", err
	}
	return digest(data), nil
}

func (v ContainerView) EditFile(path, oldText, newText, expectedSHA256 string, replaceAll bool, maxBytes int64) (hash string, err error) {
	return v.EditFileContext(context.Background(), path, oldText, newText, expectedSHA256, replaceAll, maxBytes)
}

func (v ContainerView) EditFileContext(ctx context.Context, path, oldText, newText, expectedSHA256 string, replaceAll bool, maxBytes int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if maxBytes <= 0 {
		return "", errors.New("positive edit limit required")
	}
	path, err := v.path(path)
	if err != nil {
		return "", err
	}
	target, _, err := containerMutationTarget(path)
	if err != nil {
		return "", err
	}
	lock := containerMutationLock(target)
	lock.Lock()
	defer lock.Unlock()
	file, err := openContainerExisting(path, os.O_RDONLY, containerRegular)
	if err != nil {
		return "", err
	}
	current, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return "", err
	}
	if int64(len(current)) > maxBytes {
		return "", errors.New("file exceeds edit limit")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if expectedSHA256 != "" && digest(current) != expectedSHA256 {
		return "", ErrChanged
	}
	replacement, err := applyBoundedLiteralEdit(current, oldText, newText, replaceAll, maxBytes)
	if err != nil {
		return "", err
	}
	currentTarget, mode, err := containerMutationTarget(path)
	if err != nil {
		return "", err
	}
	if currentTarget != target {
		return "", ErrChanged
	}
	if err := replaceContainerFileContext(ctx, target, replacement, mode); err != nil {
		return "", err
	}
	return digest(replacement), nil
}

// replaceContainerFile stages bytes in the destination directory so rename
// stays on one filesystem, including when /root/harness is an NFS mount.
// A rename error can also be uncertain on NFS: the server may have published
// the target before its acknowledgement was lost. Errors after rename are
// likewise uncertain because the new bytes may already be visible.
func replaceContainerFileContext(ctx context.Context, target string, data []byte, mode os.FileMode) error {
	return replaceContainerFileWithContextAndRename(ctx, target, data, mode, os.Rename)
}

func replaceContainerFileWithRename(target string, data []byte, mode os.FileMode, rename func(string, string) error) error {
	return replaceContainerFileWithContextAndRename(context.Background(), target, data, mode, rename)
}

func replaceContainerFileWithContextAndRename(ctx context.Context, target string, data []byte, mode os.FileMode, rename func(string, string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parent := filepath.Dir(target)
	tempPath, cleanup, err := stagePrivateContainerFile(parent, target, ".agent-edit-", data, mode)
	if err != nil {
		return err
	}
	defer cleanup()
	// Before publication a canceled write has not changed the target. Once
	// rename is attempted, its outcome may be uncertain on NFS and must not
	// be reclassified as a clean cancellation.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rename(tempPath, target); err != nil {
		return uncertainMutation(err)
	}
	dir, err := os.Open(parent)
	if err != nil {
		return uncertainMutation(err)
	}
	returnError := errors.Join(dir.Sync(), dir.Close())
	if returnError != nil {
		return uncertainMutation(returnError)
	}
	return nil
}

// stagePrivateContainerFile keeps incomplete content inside a sibling 0700
// directory. Both rename and hard-link publication stay on the target mount.
func stagePrivateContainerFile(parent, target, prefix string, data []byte, mode os.FileMode) (string, func(), error) {
	stagingDir, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return "", nil, err
	}
	tempPath := filepath.Join(stagingDir, filepath.Base(target)+".tmp")
	cleanup := func() {
		_ = os.Remove(tempPath)
		_ = os.Remove(stagingDir)
	}
	temp, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	writeErr := writeAll(temp, data)
	// Keep staged bytes private until they are complete. In particular, a
	// world-readable target must not expose a partially written replacement.
	var modeErr error
	if writeErr == nil {
		modeErr = temp.Chmod(mode.Perm())
	}
	var syncErr error
	if writeErr == nil && modeErr == nil {
		syncErr = temp.Sync()
	}
	closeErr := temp.Close()
	if err := errors.Join(writeErr, modeErr, syncErr, closeErr); err != nil {
		cleanup()
		return "", nil, err
	}
	return tempPath, cleanup, nil
}
