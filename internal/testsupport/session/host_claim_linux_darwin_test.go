//go:build linux || darwin

package session

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestHostFileClaimRejectsUnconfirmedAndConcurrentWriters(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	claims := HostFileClaims{Root: root}
	if _, err := claims.Acquire(context.Background(), "session-1"); err == nil {
		t.Fatal("unconfirmed host scope acquired a lock")
	}
	claims.SingleHostConfirmed = true
	release, err := claims.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := claims.Probe(context.Background(), "session-1"); !errors.Is(err, ErrHostClaimBusy) {
		t.Fatalf("probe of live writer = %v", err)
	}
	release()
	release()
	if err := claims.Probe(context.Background(), "session-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := claims.Acquire(context.Background(), "../escape"); err == nil {
		t.Fatal("unsafe lock path accepted")
	}
}

func TestHostFileClaimRejectsSymlinkAndSpecialLockTargets(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "private-locks")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	claims := HostFileClaims{Root: root, SingleHostConfirmed: true}
	target := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "symlink.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := claims.Acquire(context.Background(), "symlink"); err == nil {
		t.Fatal("symlinked lock file was accepted")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "untouched" {
		t.Fatalf("symlink target changed: %q %v", data, err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := claims.Acquire(context.Background(), "pipe"); err == nil {
		t.Fatal("FIFO lock file was accepted")
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := (HostFileClaims{Root: alias, SingleHostConfirmed: true}).Acquire(context.Background(), "normal"); err == nil {
		t.Fatal("symlinked claim root was accepted")
	}
	if _, err := (HostFileClaims{Root: alias + string(filepath.Separator), SingleHostConfirmed: true}).Acquire(context.Background(), "normal"); err == nil {
		t.Fatal("trailing slash concealed a symlinked claim root")
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := claims.Acquire(context.Background(), "normal"); err == nil {
		t.Fatal("world-visible claim root was accepted")
	}
}

func TestHostFileClaimSpansProcesses(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	claims := HostFileClaims{Root: root, SingleHostConfirmed: true}
	command := exec.Command(os.Args[0], "-test.run=^TestHostFileClaimHelper$")
	command.Env = append(os.Environ(), "HOST_CLAIM_HELPER=1", "HOST_CLAIM_ROOT="+claims.Root)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "locked" {
		_ = command.Process.Kill()
		t.Fatalf("child did not acquire claim: %v", scanner.Err())
	}
	if _, err := claims.Acquire(context.Background(), "session-1"); !errors.Is(err, ErrHostClaimBusy) {
		_ = command.Process.Kill()
		t.Fatalf("other process held claim but acquire returned %v", err)
	}
	_ = stdin.Close()
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	release, err := claims.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("claim remained locked after process exit: %v", err)
	}
	release()
}

func TestHostFileClaimHelper(t *testing.T) {
	if os.Getenv("HOST_CLAIM_HELPER") != "1" {
		return
	}
	claims := HostFileClaims{Root: os.Getenv("HOST_CLAIM_ROOT"), SingleHostConfirmed: true}
	release, err := claims.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, _ = os.Stdout.WriteString("locked\n")
	_, _ = os.Stdin.Read(make([]byte, 1))
}
