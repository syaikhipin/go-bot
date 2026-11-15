package agentfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// PresentedPath resolves a path within a Session workspace.
func PresentedPath(workspace, cwd, path string) (string, string, error) {
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(cwd) {
		return "", "", os.ErrInvalid
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(workspace, path)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", "", os.ErrPermission
	}
	return path, rel, nil
}

// OpenPresented uses the same workspace boundary at declaration and download.
func OpenPresented(workspace, rel string) (*os.File, os.FileInfo, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err == nil {
			err = errors.New("not a regular file")
		}
		return nil, nil, err
	}
	return file, info, nil
}
