package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go-harness/harness/agent"
	"go-harness/harness/jobs"
	"go-harness/harness/permission"
)

type ShellResult struct {
	ExitCode        int
	Stdout          string
	Stderr          string
	Truncated       bool
	StdoutTruncated bool
	StderrTruncated bool
	StdoutSpillPath string
	StderrSpillPath string
}

// CommandExecutor is injected only by the verified container worker. There
// is deliberately no host-process implementation in the Go Assembly.
type CommandExecutor interface {
	Run(context.Context, string, string, int) (ShellResult, error)
}

// StreamingCommandExecutor reports output while a command is still running.
// The worker implementation must use the same process-group cancellation as
// foreground Run; the Job Registry owns its context beyond the current turn.
type StreamingCommandExecutor interface {
	CommandExecutor
	RunStreaming(context.Context, string, string, int, func(string, []byte)) (ShellResult, error)
}

type Shell struct {
	Executor       CommandExecutor
	Jobs           *jobs.Registry
	DefaultWorkdir string
}

type shellArgs struct {
	Command         string `json:"command"`
	Workdir         string `json:"workdir"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	RunInBackground bool   `json:"run_in_background"`
}

func (t Shell) Spec() agent.ToolSpec {
	if t.Jobs == nil {
		return agent.ToolSpec{Name: "shellexecute", Description: "Run one foreground command inside this isolated container. Visible ordinary files, including skills under $DSH_HOME and $DSH_AGENTS_HOME, are writable. /root/harness is the user data root; in NAS-mapped mode it persists across new containers. Long output keeps its tail and may provide a temporary path to its full text. There is no managed background-job API: do not detach work with &, nohup, or setsid and assume the shell exit proves that work finished.", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"workdir":{"type":"string","description":"Optional working directory; relative paths resolve from this Session's working directory."},"timeout_seconds":{"type":"integer"}},"required":["command"]}`)}
	}
	return agent.ToolSpec{Name: "shellexecute", Description: "Run a command inside this isolated container. Set run_in_background=true to return a managed Job ID immediately; a foreground command that exceeds timeout_seconds is promoted to a Job rather than killed. Use job_output, job_list, and job_kill to track it. Do not use &, nohup, or setsid as substitutes for managed Jobs. /root/harness is the user data root; in NAS-mapped mode it persists across new containers.", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"workdir":{"type":"string","description":"Optional working directory; relative paths resolve from this Session's working directory."},"timeout_seconds":{"type":"integer"},"run_in_background":{"type":"boolean"}},"required":["command"]}`)}
}

func decodeShellArgs(raw json.RawMessage, defaultWorkdir string) (shellArgs, error) {
	var args shellArgs
	if err := decodeOneToolObject(raw, &args); err != nil {
		return args, err
	}
	if args.Workdir == "" {
		args.Workdir = defaultWorkdir
		if args.Workdir == "" {
			args.Workdir = "/root/harness"
		}
	} else if !filepath.IsAbs(args.Workdir) {
		if defaultWorkdir == "" {
			defaultWorkdir = "/root/harness"
		}
		args.Workdir = filepath.Join(defaultWorkdir, args.Workdir)
	}
	if args.TimeoutSeconds == 0 {
		args.TimeoutSeconds = 120
	}
	return args, nil
}

func (t Shell) Validate(raw json.RawMessage) error {
	if t.Executor == nil {
		return errors.New("container command executor is not available")
	}
	args, err := decodeShellArgs(raw, t.DefaultWorkdir)
	if err != nil {
		return err
	}
	if args.Command == "" || len(args.Command) > 32<<10 {
		return errors.New("command must be nonempty and at most 32768 bytes")
	}
	if args.Workdir[0] != '/' || len(args.Workdir) > 4096 {
		return errors.New("workdir must be an absolute container path")
	}
	if args.TimeoutSeconds < 1 || args.TimeoutSeconds > 600 {
		return errors.New("timeout_seconds must be between 1 and 600")
	}
	if args.RunInBackground && t.Jobs == nil {
		return errors.New("managed background Jobs are not configured")
	}
	return nil
}

func (t Shell) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	args, err := decodeShellArgs(raw, t.DefaultWorkdir)
	if err != nil {
		return "", err
	}
	if t.Jobs != nil {
		streaming, ok := t.Executor.(StreamingCommandExecutor)
		if !ok {
			return "", errors.New("container executor cannot stream managed Jobs")
		}
		return t.executeManaged(ctx, args, streaming)
	}
	return t.executeForeground(ctx, args)
}

func (t Shell) executeForeground(ctx context.Context, args shellArgs) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(args.TimeoutSeconds)*time.Second)
	defer cancel()
	result, err := t.Executor.Run(ctx, args.Command, args.Workdir, 64<<10)
	if err != nil {
		return "", err
	}
	return formatPermissionShellResult(ctx, result), nil
}

func (t Shell) executeManaged(ctx context.Context, args shellArgs, executor StreamingCommandExecutor) (string, error) {
	var result toolsShellOutcome
	var outcomeMu sync.Mutex
	job, err := t.Jobs.Start("bash", args.Command, func(jobCtx context.Context, emit func([]byte)) (int, error) {
		shellResult, runErr := executor.RunStreaming(permission.WithScope(jobCtx, permission.From(ctx)), args.Command, args.Workdir, 64<<10, func(stream string, chunk []byte) {
			// Preserve which stream produced each chunk in the bounded Job ring.
			emit(append([]byte(stream+": "), chunk...))
		})
		outcomeMu.Lock()
		result = toolsShellOutcome{result: shellResult, err: runErr}
		outcomeMu.Unlock()
		return shellResult.ExitCode, runErr
	})
	if err != nil {
		// A full Job Registry cannot promote a foreground command, but it
		// must not prevent an ordinary shell call. Run it under the usual
		// deadline instead; explicit background work still needs a Job slot.
		if errors.Is(err, jobs.ErrCapacity) && !args.RunInBackground {
			return t.executeForeground(ctx, args)
		}
		return "", err
	}
	if args.RunInBackground {
		status, err := t.Jobs.MarkVisible(job.ID)
		if err != nil {
			return "", err
		}
		return "job_id=" + job.ID + " status=" + string(status.Status), nil
	}
	status, err := t.Jobs.Wait(ctx, job.ID, time.Duration(args.TimeoutSeconds)*time.Second)
	if err != nil {
		// A cancelled foreground call must not leave an invisible live job.
		_, _ = t.Jobs.Kill(job.ID)
		_, _ = t.Jobs.Wait(context.Background(), job.ID, 10*time.Second)
		return "", err
	}
	if status.Status == jobs.Running || status.Status == jobs.Stopping {
		output, err := t.Jobs.Consume(job.ID)
		if err != nil {
			return "", err
		}
		loss := ""
		if output.Lossy {
			loss = " output_lossy=true"
		}
		if _, err := t.Jobs.MarkVisible(job.ID); err != nil {
			return "", err
		}
		return fmt.Sprintf("job_id=%s status=running promoted_after=%ds%s\n%s", job.ID, args.TimeoutSeconds, loss, output.Text), nil
	}
	defer t.Jobs.Remove(job.ID)
	outcomeMu.Lock()
	completed := result
	outcomeMu.Unlock()
	if completed.err != nil {
		return "", completed.err
	}
	return formatPermissionShellResult(ctx, completed.result), nil
}

type toolsShellOutcome struct {
	result ShellResult
	err    error
}

func formatPermissionShellResult(ctx context.Context, result ShellResult) string {
	text := formatShellResult(result)
	scope := permission.From(ctx)
	if !scope.Restricted() || result.ExitCode == 0 {
		return text
	}
	stderr := strings.ToLower(result.Stderr)
	if strings.Contains(stderr, "permission denied") || strings.Contains(stderr, "invalid cross-device link") {
		text += "\n" + permission.Denial(scope.Mode, scope.Ask, "command")
	}
	return text
}

func formatShellResult(result ShellResult) string {
	stdoutNotice := ""
	if result.StdoutTruncated {
		stdoutNotice = "\n[output truncated; full output: " + shellSpillLabel(result.StdoutSpillPath) + "]"
	}
	stderrNotice := ""
	if result.StderrTruncated {
		stderrNotice = "\n[output truncated; full output: " + shellSpillLabel(result.StderrSpillPath) + "]"
	}
	return fmt.Sprintf("exit_code=%d truncated=%t\nstdout:\n%s%s\nstderr:\n%s%s", result.ExitCode, result.Truncated, result.Stdout, stdoutNotice, result.Stderr, stderrNotice)
}

func shellSpillLabel(path string) string {
	if path == "" {
		return "(unavailable)"
	}
	return path
}
