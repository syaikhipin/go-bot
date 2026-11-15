package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

const browserCLI = "/usr/local/bin/agent-browser"

type browserSession struct {
	socketDir string
}

func newBrowserSession() (browserSession, error) {
	// A short absolute path leaves room for named Unix sockets.
	socketDir, err := os.MkdirTemp("/tmp", "ghb-")
	return browserSession{socketDir: socketDir}, err
}

func (b browserSession) environment() []string {
	if b.socketDir == "" {
		return nil
	}
	return []string{
		"AGENT_BROWSER_SOCKET_DIR=" + b.socketDir,
		"AGENT_BROWSER_CONTENT_BOUNDARIES=1",
		"AGENT_BROWSER_MAX_OUTPUT=20000",
		"AGENT_BROWSER_IDLE_TIMEOUT_MS=1800000",
	}
}

func (b browserSession) close() error {
	if b.socketDir == "" {
		return nil
	}
	entries, err := os.ReadDir(b.socketDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, browserCLI, "close", "--all")
		cmd.Env = append(shellEnvironment(), b.environment()...)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("close session browsers: %w", err)
		}
	}
	return os.RemoveAll(b.socketDir)
}
