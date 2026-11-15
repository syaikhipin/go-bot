//go:build linux || darwin

package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var ErrHostClaimBusy = errors.New("Session writer is still active on this host")

// HostFileClaims serializes one Session across processes on one physical host.
// Root must be a host-local, control-plane-private directory, never an OSS
// mount or an Agent-visible workspace. This is not cross-ECS fencing.
type HostFileClaims struct {
	Root                string
	SingleHostConfirmed bool
}

func (g HostFileClaims) Acquire(ctx context.Context, sessionID string) (func(), error) {
	if !g.SingleHostConfirmed || !filepath.IsAbs(g.Root) || filepath.Clean(g.Root) != g.Root || filepath.Clean(g.Root) == string(filepath.Separator) || !safePathComponent.MatchString(sessionID) {
		return nil, errors.New("host-local Session claim is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(g.Root, 0o700); err != nil {
		return nil, fmt.Errorf("create host claim directory: %w", err)
	}
	rootInfo, err := os.Lstat(g.Root)
	if err != nil {
		return nil, fmt.Errorf("inspect host claim directory: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("host claim directory must be a private real directory (mode %s)", rootInfo.Mode())
	}
	file, err := os.OpenFile(filepath.Join(g.Root, sessionID+".lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open host Session claim: %w", err)
	}
	fileInfo, err := file.Stat()
	if err != nil || !fileInfo.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("host Session claim must be a regular file")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrHostClaimBusy
		}
		return nil, fmt.Errorf("lock host Session claim: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			_ = file.Close()
		})
	}, nil
}

func (g HostFileClaims) Probe(ctx context.Context, sessionID string) error {
	release, err := g.Acquire(ctx, sessionID)
	if err != nil {
		return err
	}
	release()
	return nil
}
