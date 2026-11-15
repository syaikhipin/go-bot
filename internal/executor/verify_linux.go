//go:build linux

package executor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// VerifyHostRuntime checks the Host storage layout. Container mode uses the
// instance's writable layer. Mounted mode requires a writable mount
// at /root/harness for a Docker volume or NAS.
func VerifyHostRuntime(mode string) error {
	if mode != "container" && mode != "mounted" {
		return errors.New("unknown user Host runtime mode")
	}
	if os.Geteuid() != 0 {
		return errors.New("user Host must run as root")
	}
	info, err := os.Lstat("/root/harness")
	if err != nil {
		return fmt.Errorf("inspect /root/harness: %w", err)
	}
	if !info.IsDir() {
		return errors.New("/root/harness must be a real directory")
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("read mount namespace: %w", err)
	}
	if err := verifyHostMountInfo(string(data), mode); err != nil {
		return err
	}
	if err := probeHostFileSemantics("/root/harness"); err != nil {
		return fmt.Errorf("user Host data filesystem lacks required file operations: %w", err)
	}
	return verifyNoMountPrivilege()
}

func verifyHostMountInfo(data, mode string) error {
	var harnessMount, writable bool
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		mountpoint := unescapeMountPath(fields[4])
		if mountpoint == "/root/harness" {
			harnessMount = true
			writable = strings.Contains(","+fields[5]+",", ",rw,")
		}

	}
	if mode == "mounted" && (!harnessMount || !writable) {
		return errors.New("user Host requires a writable mount at /root/harness")
	}
	if mode == "container" && harnessMount {
		return errors.New("user Host must use its container writable layer in container mode")
	}
	return nil
}

// The Host's Session journal, immutable attachments, and Schedule file all
// rely on these same-directory operations. A writable NAS mount alone does
// not prove that their publish and durability paths will work. This is a
// capability check, not a substitute for real NFS failure/recovery testing.
func probeHostFileSemantics(directory string) error {
	stage, err := os.CreateTemp(directory, ".serveragent-fs-probe-")
	if err != nil {
		return err
	}
	defer func() {
		_ = stage.Close()
		_ = os.Remove(stage.Name())
	}()
	linked := stage.Name() + ".linked"
	defer os.Remove(linked)
	first := []byte("original")
	if written, writeErr := stage.Write(first); writeErr != nil || written != len(first) {
		return errors.Join(writeErr, fmt.Errorf("write staged file: %d of %d bytes", written, len(first)))
	}
	if err := errors.Join(stage.Sync(), stage.Close()); err != nil {
		return fmt.Errorf("sync staged file: %w", err)
	}
	if err := os.Link(stage.Name(), linked); err != nil {
		return fmt.Errorf("publish no-replace hard link: %w", err)
	}
	if err := os.Link(stage.Name(), linked); !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("hard link did not reject an existing target: %v", err)
	}
	if err := syncHostProbeDirectory(directory); err != nil {
		return fmt.Errorf("sync published hard link: %w", err)
	}
	if data, err := os.ReadFile(linked); err != nil || !bytes.Equal(data, first) {
		return fmt.Errorf("read published hard link: %w", errors.Join(err, errors.New("content mismatch")))
	}

	replacement, err := os.CreateTemp(directory, ".serveragent-fs-probe-")
	if err != nil {
		return err
	}
	defer func() {
		_ = replacement.Close()
		_ = os.Remove(replacement.Name())
	}()
	second := []byte("replacement")
	if written, writeErr := replacement.Write(second); writeErr != nil || written != len(second) {
		_ = replacement.Close()
		return errors.Join(writeErr, fmt.Errorf("write replacement file: %d of %d bytes", written, len(second)))
	}
	if err := errors.Join(replacement.Sync(), replacement.Close()); err != nil {
		return fmt.Errorf("sync replacement file: %w", err)
	}
	if err := os.Rename(replacement.Name(), linked); err != nil {
		return fmt.Errorf("replace file by same-directory rename: %w", err)
	}
	if err := syncHostProbeDirectory(directory); err != nil {
		return fmt.Errorf("sync renamed file: %w", err)
	}
	if data, err := os.ReadFile(linked); err != nil || !bytes.Equal(data, second) {
		return fmt.Errorf("read renamed file: %w", errors.Join(err, errors.New("content mismatch")))
	}
	return nil
}

func syncHostProbeDirectory(directory string) error {
	file, err := os.Open(filepath.Clean(directory))
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func unescapeMountPath(path string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(path)
}

func verifyNoMountPrivilege() error {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		value, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
		if err != nil {
			return err
		}
		if value&(uint64(1)<<unix.CAP_SYS_ADMIN) != 0 {
			return errors.New("container worker retains CAP_SYS_ADMIN")
		}
		return nil
	}
	return errors.New("container effective capabilities are unavailable")
}
