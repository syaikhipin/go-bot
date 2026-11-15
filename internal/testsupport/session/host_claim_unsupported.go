//go:build !linux && !darwin

package session

import (
	"context"
	"errors"
)

var ErrHostClaimBusy = errors.New("Session writer is still active on this host")

type HostFileClaims struct {
	Root                string
	SingleHostConfirmed bool
}

func (HostFileClaims) Acquire(context.Context, string) (func(), error) {
	return nil, errors.New("host Session claims require Linux or macOS")
}

func (HostFileClaims) Probe(context.Context, string) error {
	return errors.New("host Session claims require Linux or macOS")
}
