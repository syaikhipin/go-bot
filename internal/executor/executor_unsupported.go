//go:build !linux

package executor

import (
	"context"
	"errors"

	"go-bot/harness/tools"
)

func (Executor) Run(context.Context, string, string, int) (tools.ShellResult, error) {
	return tools.ShellResult{}, errors.New("container shell requires Linux")
}
