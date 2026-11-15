//go:build !linux

package agentfs

import (
	"errors"
	"os"
	"path/filepath"
)

func ensureContainerParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

// The production container worker runs on Linux. This implementation keeps
// native-path behavior testable on other developer platforms; it is not the
// Linux worker's stable-handle security check.
func openContainerExisting(path string, flags int, kind containerOpenKind) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if (kind == containerRegular && !info.Mode().IsRegular()) ||
		(kind == containerDirectory && !info.IsDir()) ||
		(kind == containerRegularOrDirectory && !info.Mode().IsRegular() && !info.IsDir()) {
		return nil, errors.New("expected a regular file or directory")
	}
	return os.OpenFile(path, flags, 0)
}

func containerMutationTarget(path string) (string, os.FileMode, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, linkErr := os.Lstat(path); linkErr == nil {
			return "", 0, errors.New("existing file target is unavailable")
		} else if !errors.Is(linkErr, os.ErrNotExist) {
			return "", 0, linkErr
		}
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(path))
		if parentErr != nil {
			return "", 0, parentErr
		}
		return filepath.Join(parent, filepath.Base(path)), 0o600, nil
	}
	if err != nil {
		return "", 0, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, errors.New("expected a regular file")
	}
	return resolved, info.Mode().Perm(), nil
}
