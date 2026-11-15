//go:build linux

package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"go-harness/harness/permission"
	"go-harness/internal/agentfs"
	"golang.org/x/sys/unix"
)

const landlockWrite = unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
	unix.LANDLOCK_ACCESS_FS_REMOVE_FILE | unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
	unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
	unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK | unix.LANDLOCK_ACCESS_FS_MAKE_SYM | unix.LANDLOCK_ACCESS_FS_REFER |
	unix.LANDLOCK_ACCESS_FS_TRUNCATE

func restrictThread(scope permission.Scope, temp string) error {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return errno
	}
	if abi < 6 {
		return fmt.Errorf("Landlock ABI 6 is required, found %d", abi)
	}
	rights := uint64(landlockWrite | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV)
	if abi >= 9 {
		rights |= unix.LANDLOCK_ACCESS_FS_RESOLVE_UNIX
	}
	attr := [3]uint64{rights, 0, unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return errno
	}
	defer unix.Close(int(fd))
	add := func(path string, access uint64, directory bool) error {
		flags := unix.O_PATH | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if directory {
			flags |= unix.O_DIRECTORY
		}
		parent, err := unix.Open(path, flags, 0)
		if err != nil {
			return err
		}
		defer unix.Close(parent)
		if directory {
			resolved, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", parent))
			if err != nil {
				return err
			}
			if resolved != filepath.Clean(path) {
				return errors.New("writable sandbox root must be a physical directory")
			}
		}
		// landlock_path_beneath_attr is a packed u64 followed by a signed fd.
		rule := [3]uint32{}
		*(*uint64)(unsafe.Pointer(&rule[0])) = access
		rule[2] = uint32(parent)
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		if errno != 0 {
			return errno
		}
		return nil
	}
	for _, sink := range []string{"/dev/null", "/dev/zero", "/dev/urandom", "/dev/random"} {
		if err := add(sink, unix.LANDLOCK_ACCESS_FS_WRITE_FILE, false); err != nil {
			return err
		}
	}
	if scope.Mode == permission.WorkspaceWrite {
		if !filepath.IsAbs(scope.Root) || scope.Root == "/" {
			return errors.New("invalid writable workspace")
		}
		access := rights &^ uint64(unix.LANDLOCK_ACCESS_FS_IOCTL_DEV|unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK|unix.LANDLOCK_ACCESS_FS_MAKE_CHAR)
		if err := add(scope.Root, access, true); err != nil {
			return err
		}
		if temp != "" {
			if err := add(temp, access, true); err != nil {
				return err
			}
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

const sandboxRunner = "__go_harness_landlock"
const sandboxFailurePrefix = "[go-harness sandbox setup] "

type fileEffectResult struct {
	Error       string
	Denied      bool
	Unknown     bool
	Changed     bool
	NotObserved bool
}

func init() {
	if len(os.Args) < 2 || os.Args[1] != sandboxRunner {
		return
	}
	runtime.LockOSThread()
	if len(os.Args) < 6 {
		os.Exit(125)
	}
	action, mode, root, temp := os.Args[2], permission.Mode(os.Args[3]), os.Args[4], os.Args[5]
	if err := restrictThread(permission.Scope{Mode: mode, Root: root}, temp); err != nil {
		fmt.Fprintln(os.Stderr, sandboxFailurePrefix+err.Error())
		os.Exit(125)
	}
	switch action {
	case "probe":
		os.Exit(0)
	case "shell":
		if len(os.Args) != 7 {
			os.Exit(125)
		}
		fmt.Fprintln(os.Stderr, sandboxFailurePrefix+unix.Exec("/bin/sh", []string{"/bin/sh", "-c", os.Args[6]}, os.Environ()).Error())
		os.Exit(125)
	case "mutate":
		var effect agentfs.FileEffect
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 8<<20)).Decode(&effect); err != nil {
			os.Exit(125)
		}
		err := agentfs.ApplyFileEffect(context.Background(), effect)
		result := fileEffectResult{}
		if err != nil {
			result = fileEffectResult{Error: err.Error(), Denied: errors.Is(err, os.ErrPermission) || errors.Is(err, unix.EXDEV),
				Unknown: errors.Is(err, agentfs.ErrMutationOutcomeUnknown), Changed: errors.Is(err, agentfs.ErrChanged),
				NotObserved: errors.Is(err, agentfs.ErrNotObserved)}
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			os.Exit(125)
		}
		os.Exit(0)
	default:
		os.Exit(125)
	}
}

func sandboxCommand(ctx context.Context, action, temp string, extra ...string) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	scope := permission.From(ctx)
	args := []string{sandboxRunner, action, string(scope.Mode), scope.Root, temp}
	args = append(args, extra...)
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/root", "LANG=C.UTF-8"}
	return cmd, nil
}

func sandboxUnavailable(detail string) error {
	return permission.Failure{Code: "SANDBOX_UNAVAILABLE", Message: "Linux file sandbox unavailable: " + strings.TrimSpace(detail)}
}

func probeSandbox(ctx context.Context) error {
	cmd, err := sandboxCommand(ctx, "probe", "")
	if err != nil {
		return err
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return sandboxUnavailable(string(output) + err.Error())
	}
	return nil
}

func confineMutation(ctx context.Context) context.Context {
	return agentfs.WithFileEffects(ctx, func(ctx context.Context, effect agentfs.FileEffect) error {
		cmd, err := sandboxCommand(ctx, "mutate", "")
		if err != nil {
			return err
		}
		input, err := json.Marshal(effect)
		if err != nil {
			return err
		}
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil {
			if strings.HasPrefix(stderr.String(), sandboxFailurePrefix) {
				return sandboxUnavailable(stderr.String())
			}
			return errors.Join(agentfs.ErrMutationOutcomeUnknown, err)
		}
		var result fileEffectResult
		if err := json.Unmarshal(output, &result); err != nil {
			return errors.Join(agentfs.ErrMutationOutcomeUnknown, err)
		}
		if result.Error == "" {
			return nil
		}
		err = errors.New(result.Error)
		switch {
		case result.Denied:
			return errors.Join(os.ErrPermission, err)
		case result.Unknown:
			return errors.Join(agentfs.ErrMutationOutcomeUnknown, err)
		case result.Changed:
			return errors.Join(agentfs.ErrChanged, err)
		case result.NotObserved:
			return errors.Join(agentfs.ErrNotObserved, err)
		default:
			return err
		}
	})
}

func shellSandboxTemp(ctx context.Context) (string, error) {
	if permission.From(ctx).Mode != permission.WorkspaceWrite {
		return "", nil
	}
	return os.MkdirTemp("/tmp", "go-harness-sandbox-")
}
