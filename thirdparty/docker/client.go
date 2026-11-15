// Package docker wraps Docker CLI calls. Secrets are passed by environment name,
// never embedded in docker run arguments, and inspect output is never logged.
package docker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type Client struct{}

func (Client) Run(ctx context.Context, env map[string]string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := stderr.String()
		if len(msg) > 1000 {
			msg = msg[:1000]
		}
		for _, secret := range env {
			if secret != "" {
				msg = strings.ReplaceAll(msg, secret, "[redacted]")
			}
		}
		if msg == "" {
			msg = "Docker command failed"
		}
		return nil, fmt.Errorf("docker: %s", strings.TrimSpace(msg))
	}
	return out.Bytes(), nil
}
