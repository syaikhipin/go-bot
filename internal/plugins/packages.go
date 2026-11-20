package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"go-bot/plugin"
)

type Packages struct {
	Directory string
	Node      string
	Main      string
	PNPM      string
}

type packageRequest struct {
	Method      string            `json:"method"`
	Directory   string            `json:"directory"`
	PNPM        string            `json:"pnpm,omitempty"`
	Name        string            `json:"name,omitempty"`
	Builds      []string          `json:"builds,omitempty"`
	Source      plugin.Source     `json:"source,omitempty"`
	Credentials map[string]string `json:"credentials,omitempty"`
}

type limitedOutput struct {
	data     bytes.Buffer
	overflow bool
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	if b.data.Len()+len(p) > maxFrame {
		b.overflow = true
		return len(p), nil
	}
	return b.data.Write(p)
}

// One bounded subprocess runs package operations without loading business
// modules. The enclosing Host manager serializes mutations and owns activation.
func (p Packages) call(ctx context.Context, request packageRequest, output any) error {
	if !filepath.IsAbs(p.Directory) || !filepath.IsAbs(p.Main) {
		return errors.New("package manager paths must be absolute")
	}
	if err := os.MkdirAll(p.Directory, 0700); err != nil {
		return err
	}
	request.Directory, request.PNPM = p.Directory, p.PNPM
	input, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(input) > 4<<20 {
		return errors.New("package request exceeds 4 MiB")
	}
	node := p.Node
	if node == "" {
		node = "node"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, node, p.Main)
	command.Dir = p.Directory
	command.Env = runtimeEnvironment()
	command.Stdin = bytes.NewReader(input)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = 3 * time.Second
	var result limitedOutput
	command.Stdout = &result
	// stderr can contain third-party exception data. Only the structured and
	// redacted response on stdout is returned through the public management API.
	err = command.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if result.overflow {
		return failure("PACKAGE_OUTPUT_LIMIT", "Package manager response exceeds 1 MiB")
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  *plugin.Error   `json:"error"`
	}
	if json.Unmarshal(result.data.Bytes(), &response) != nil {
		return failure("PACKAGE_RUNTIME_FAILED", "Package manager returned no valid structured response")
	}
	if response.Error != nil {
		return response.Error
	}
	if err != nil {
		return fmt.Errorf("package manager failed: %w", err)
	}
	if output != nil {
		return json.Unmarshal(response.Result, output)
	}
	return nil
}

func (p Packages) Inspect(ctx context.Context, name string, source plugin.Source, credentials map[string]string) (plugin.PackageInfo, error) {
	var result plugin.PackageInfo
	err := p.call(ctx, packageRequest{Method: "inspect", Name: name, Source: source, Credentials: credentials}, &result)
	return result, err
}
func (p Packages) Install(ctx context.Context, name string, source plugin.Source, credentials map[string]string, builds ...string) (plugin.PackageInfo, error) {
	var result plugin.PackageInfo
	err := p.call(ctx, packageRequest{Method: "install", Name: name, Source: source, Credentials: credentials, Builds: builds}, &result)
	return result, err
}
func (p Packages) List(ctx context.Context) ([]plugin.PackageInfo, error) {
	var result []plugin.PackageInfo
	err := p.call(ctx, packageRequest{Method: "list"}, &result)
	return result, err
}
func (p Packages) Remove(ctx context.Context, name string) error {
	return p.call(ctx, packageRequest{Method: "remove", Name: name}, nil)
}
