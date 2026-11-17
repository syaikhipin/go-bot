//go:build !linux

package executor

import (
	"context"

	"go-bot/harness/permission"
	"go-bot/internal/agentfs"
)

func probeSandbox(context.Context) error {
	return permission.Failure{Code: "SANDBOX_UNAVAILABLE", Message: "Linux file sandbox unavailable"}
}

func confineMutation(ctx context.Context) context.Context {
	return agentfs.WithFileEffects(ctx, func(ctx context.Context, _ agentfs.FileEffect) error { return probeSandbox(ctx) })
}
