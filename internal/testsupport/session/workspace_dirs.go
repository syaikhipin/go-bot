package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureSessionWorkspaceOnly creates only the live workspace beneath the dedicated
// mount. A symlink at any component is rejected before a descendant is made,
// so a stale or malicious session tree cannot redirect the Agent's bind mount
// or trusted Skill import into another workspace or a host path.
func EnsureSessionWorkspaceOnly(mountRoot, userID, sessionID string) (string, error) {
	directory, err := SessionDirectory(mountRoot, userID, sessionID)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(mountRoot)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("Session mount root must be a real directory")
	}
	root, err := os.OpenRoot(mountRoot)
	if err != nil {
		return "", err
	}
	defer root.Close()
	components := []string{"users", userID, "sessions", sessionID}
	var relative string
	for _, component := range components {
		relative = filepath.Join(relative, component)
		if err := ensureRealDirectory(root, relative); err != nil {
			return "", err
		}
	}
	if err := ensureRealDirectory(root, filepath.Join(relative, "workspace")); err != nil {
		return "", err
	}
	return filepath.Join(directory, "workspace"), nil
}

// OpenExistingSessionWorkspace opens a historical workspace without creating
// any path. Each component must be a real directory; a Session-controlled
// symlink must not redirect a control-plane file download to another Session
// or outside the dedicated mount.
func OpenExistingSessionWorkspace(mountRoot, userID, sessionID string) (*os.Root, error) {
	if _, err := SessionDirectory(mountRoot, userID, sessionID); err != nil {
		return nil, err
	}
	info, err := os.Lstat(mountRoot)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("Session mount root must be a real directory")
	}
	root, err := os.OpenRoot(mountRoot)
	if err != nil {
		return nil, err
	}
	for _, component := range []string{"users", userID, "sessions", sessionID, "workspace"} {
		info, err := root.Lstat(component)
		if err != nil {
			_ = root.Close()
			return nil, err
		}
		if !info.IsDir() {
			_ = root.Close()
			return nil, fmt.Errorf("Session path %s must be a real directory", component)
		}
		child, err := root.OpenRoot(component)
		if err != nil {
			_ = root.Close()
			return nil, err
		}
		_ = root.Close()
		root = child
	}
	return root, nil
}

func ensureRealDirectory(root *os.Root, relative string) error {
	if err := root.Mkdir(relative, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create Session directory %s: %w", relative, err)
	}
	info, err := root.Lstat(relative)
	if err != nil {
		return fmt.Errorf("inspect Session directory %s: %w", relative, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Session path %s is not a real directory", relative)
	}
	return nil
}
