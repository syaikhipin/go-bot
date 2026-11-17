//go:build linux

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/permission"
	"go-bot/harness/tools"
)

const shellTerminationGrace = 2 * time.Second

// Run executes only inside a worker that passed VerifyRuntime. The initial
// cwd is not a sandbox; OS mounts and the outer container constrain effects.
func (e Executor) Run(ctx context.Context, command, workdir string, maxOutput int) (tools.ShellResult, error) {
	return e.RunStreaming(ctx, command, workdir, maxOutput, nil)
}

type shellStreamWriter struct {
	stream string
	output *shellOutputCollector
	emit   func(string, []byte)
}

func (w shellStreamWriter) Write(data []byte) (int, error) {
	n, err := w.output.Write(data)
	if n > 0 && w.emit != nil {
		w.emit(w.stream, append([]byte(nil), data[:n]...))
	}
	return n, err
}

// RunStreaming emits bounded output chunks while the command is alive. The
// callback may be called concurrently for stdout and stderr.
func (e Executor) RunStreaming(ctx context.Context, command, workdir string, maxOutput int, emit func(string, []byte)) (tools.ShellResult, error) {
	if !e.verified {
		return tools.ShellResult{}, errors.New("container command executor was not built by verified worker")
	}
	if command == "" || !filepath.IsAbs(workdir) || maxOutput <= 0 || maxOutput > 1<<20 {
		return tools.ShellResult{}, errors.New("invalid container command, workdir or output limit")
	}
	if err := ctx.Err(); err != nil {
		return tools.ShellResult{}, err
	}
	temp, err := shellSandboxTemp(ctx)
	if err != nil {
		return tools.ShellResult{}, err
	}
	if temp != "" {
		defer os.RemoveAll(temp)
	}
	var cmd *exec.Cmd
	if permission.From(ctx).Restricted() {
		cmd, err = sandboxCommand(ctx, "shell", temp, command)
		if err != nil {
			return tools.ShellResult{}, err
		}
		// A confined command must not connect to a browser daemon from a
		// preceding full-access invocation or inherit Host service credentials.
		if temp != "" {
			cmd.Env = append(cmd.Env, "TMPDIR="+temp, "AGENT_BROWSER_SOCKET_DIR="+temp)
		}
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.Env = append(shellEnvironment(), e.environment...)
	}
	cmd.Dir = workdir
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = shellTerminationGrace
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	stdout := &shellOutputCollector{limit: maxOutput}
	stderr := &shellOutputCollector{limit: maxOutput}
	cmd.Stdout = shellStreamWriter{stream: "stdout", output: stdout, emit: emit}
	cmd.Stderr = shellStreamWriter{stream: "stderr", output: stderr, emit: emit}
	err = cmd.Run()
	var cleanupErr error
	if ctx.Err() != nil && cmd.Process != nil {
		cleanupErr = waitForManagedGroupExit(cmd.Process.Pid, shellTerminationGrace)
	}
	result := tools.ShellResult{
		Stdout: string(stdout.tail), Stderr: string(stderr.tail),
		Truncated:       stdout.truncated || stderr.truncated,
		StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated,
		StdoutSpillPath: stdout.finish(), StderrSpillPath: stderr.finish(),
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
	}
	if ctx.Err() != nil {
		if cleanupErr != nil {
			return result, errors.Join(ctx.Err(), agent.ErrToolOutcomeUnknown, cleanupErr)
		}
		return result, ctx.Err()
	}
	if err == nil {
		result.ExitCode = 0
		return result, nil
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return result, errors.Join(agent.ErrToolOutcomeUnknown,
			fmt.Errorf("command parent exited but an output pipe remained open; descendant effects may be unknown: %w", err))
	}
	if exit != nil {
		if permission.From(ctx).Restricted() && result.ExitCode == 125 && strings.HasPrefix(result.Stderr, sandboxFailurePrefix) {
			return result, sandboxUnavailable(result.Stderr)
		}
		return result, nil
	}
	return result, err
}

// The direct shell can exit before a same-group child. Cancellation waits a
// short grace after SIGTERM, then escalates against the whole process group.
// A process that deliberately leaves that group is outside this contract.
func waitForManagedGroupExit(pgid int, grace time.Duration) error {
	if gone, err := waitForGroupAbsence(pgid, grace); err != nil || gone {
		return err
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill managed process group %d: %w", pgid, err)
	}
	if gone, err := waitForGroupAbsence(pgid, grace); err != nil || gone {
		return err
	}
	return fmt.Errorf("managed process group %d still present after SIGKILL", pgid)
}

func waitForGroupAbsence(pgid int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect managed process group %d: %w", pgid, err)
		}
		live, err := linuxGroupHasLiveMembers(pgid)
		if err != nil {
			return false, fmt.Errorf("inspect live managed process group %d: %w", pgid, err)
		}
		if !live {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// kill(-pgid, 0) also sees zombies until another process reaps them. They
// cannot perform work and must not turn successful escalation into an
// unknown-outcome error. Match the Linux /proc group membership by pgrp,
// rather than assuming the group leader is still alive.
func linuxGroupHasLiveMembers(pgid int) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			continue // exited between listing and read
		}
		if err != nil {
			return false, err
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return false, fmt.Errorf("malformed /proc/%s/stat", entry.Name())
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 3 {
			return false, fmt.Errorf("short /proc/%s/stat", entry.Name())
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, err
		}
		if group == pgid && fields[0] != "Z" && fields[0] != "X" {
			return true, nil
		}
	}
	return false, nil
}
