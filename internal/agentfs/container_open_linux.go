//go:build linux

package agentfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Create missing parents through validated directory descriptors. A plain
// MkdirAll(path) could follow /proc/self/fd/<worker-fd> and mutate its target
// before the file-tool operation gets a chance to reject the magic link.
func ensureContainerParent(path string) error {
	parent := filepath.Dir(path)
	fd, err := unix.Openat2(unix.AT_FDCWD, "/", &unix.OpenHow{
		Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := checkContainerFD(fd, containerDirectory); err != nil {
		return err
	}
	if parent == "/" {
		return nil
	}
	for _, component := range strings.Split(strings.TrimPrefix(parent, "/"), "/") {
		next, openErr := unix.Openat2(fd, component, &unix.OpenHow{
			Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_MAGICLINKS,
		})
		if errors.Is(openErr, unix.ENOENT) {
			if err := unix.Mkdirat(fd, component, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				return err
			}
			next, openErr = unix.Openat2(fd, component, &unix.OpenHow{
				Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_MAGICLINKS,
			})
		}
		if openErr != nil {
			return openErr
		}
		if err := checkContainerFD(next, containerDirectory); err != nil {
			_ = unix.Close(next)
			return err
		}
		_ = unix.Close(fd)
		fd = next
	}
	return nil
}

// A container's native filesystem is intentionally not confined to its
// workspace. File tools still must not open worker-private procfs handles,
// kernel interfaces or special files as if they were task documents.
// openat2 rejects procfs magic links before opening a stable O_PATH handle;
// reopening that handle prevents a symlink swap between validation and use.
func openContainerExisting(path string, flags int, kind containerOpenKind) (*os.File, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	if err := checkContainerFD(fd, kind); err != nil {
		return nil, fmt.Errorf("unsafe container file %q: %w", path, err)
	}
	return os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", fd), flags, 0)
}

// Resolve ordinary symlinks to their targets before an atomic rename. Renaming
// over the symlink itself would change its meaning and leave the old target
// untouched, unlike the previous in-place write behavior.
func containerMutationTarget(path string) (string, os.FileMode, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_MAGICLINKS,
	})
	if errors.Is(err, unix.ENOENT) {
		// An existing dangling symlink is not a new file. Replacing the link
		// would silently differ from following its target.
		if _, linkErr := os.Lstat(path); linkErr == nil {
			return "", 0, errors.New("existing file target is unavailable")
		} else if !errors.Is(linkErr, os.ErrNotExist) {
			return "", 0, linkErr
		}
		parent := filepath.Dir(path)
		parentFD, parentErr := unix.Openat2(unix.AT_FDCWD, parent, &unix.OpenHow{
			Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_MAGICLINKS,
		})
		if parentErr != nil {
			return "", 0, parentErr
		}
		defer unix.Close(parentFD)
		if err := checkContainerFD(parentFD, containerDirectory); err != nil {
			return "", 0, err
		}
		resolved, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", parentFD))
		if err != nil {
			return "", 0, err
		}
		return filepath.Join(resolved, filepath.Base(path)), 0o600, nil
	}
	if err != nil {
		return "", 0, err
	}
	defer unix.Close(fd)
	if err := checkContainerFD(fd, containerRegular); err != nil {
		return "", 0, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", 0, err
	}
	resolved, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return "", 0, err
	}
	if strings.HasSuffix(resolved, " (deleted)") {
		return "", 0, ErrChanged
	}
	return resolved, os.FileMode(stat.Mode & 0o777), nil
}

func checkContainerFD(fd int, kind containerOpenKind) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		if kind == containerDirectory {
			return errors.New("expected a directory")
		}
	case unix.S_IFDIR:
		if kind == containerRegular {
			return errors.New("expected a regular file")
		}
	default:
		return errors.New("special files are not task files")
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return err
	}
	switch fs.Type {
	case unix.PROC_SUPER_MAGIC, unix.SYSFS_MAGIC, unix.DEVPTS_SUPER_MAGIC,
		unix.CGROUP_SUPER_MAGIC, unix.CGROUP2_SUPER_MAGIC, unix.DEBUGFS_MAGIC,
		unix.SECURITYFS_MAGIC, unix.TRACEFS_MAGIC:
		return errors.New("kernel pseudo-filesystems are not task files")
	}
	return nil
}
