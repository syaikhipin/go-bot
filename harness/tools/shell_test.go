package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go-bot/harness/jobs"
)

type shellResultStub struct{ result ShellResult }

func (s shellResultStub) Run(context.Context, string, string, int) (ShellResult, error) {
	return s.result, nil
}

type shellWorkdirStub struct{ cwd string }

func (s *shellWorkdirStub) Run(_ context.Context, _, workdir string, _ int) (ShellResult, error) {
	s.cwd = workdir
	return ShellResult{}, nil
}

func TestShellUsesAgentDefaultWorkdirUnlessExplicitlyOverridden(t *testing.T) {
	stub := &shellWorkdirStub{}
	tool := Shell{Executor: stub, DefaultWorkdir: "/workspace/research"}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"pwd"}`)); err != nil || stub.cwd != "/workspace/research" {
		t.Fatalf("default cwd = %q, %v", stub.cwd, err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"pwd","workdir":"/tmp"}`)); err != nil || stub.cwd != "/tmp" {
		t.Fatalf("explicit cwd = %q, %v", stub.cwd, err)
	}
	if err := tool.Validate(json.RawMessage(`{"command":"pwd","workdir":"../drafts"}`)); err != nil {
		t.Fatalf("relative workdir rejected: %v", err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"pwd","workdir":"../drafts"}`)); err != nil || stub.cwd != "/workspace/drafts" {
		t.Fatalf("relative cwd = %q, %v", stub.cwd, err)
	}
}

func TestShellShowsTailAndCompleteOutputAvailability(t *testing.T) {
	tool := Shell{Executor: shellResultStub{result: ShellResult{
		ExitCode: 7, Stdout: "last stdout line", Stderr: "last error line",
		Truncated: true, StdoutTruncated: true, StderrTruncated: true,
		StdoutSpillPath: "/tmp/full-stdout",
	}}}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"example"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"exit_code=7", "last stdout line", "last error line", "full output: /tmp/full-stdout", "full output: (unavailable)"} {
		if !strings.Contains(output, part) {
			t.Fatalf("shell output missing %q: %s", part, output)
		}
	}
}

func TestShellSpecDoesNotPromiseManagedBackgroundWork(t *testing.T) {
	description := (Shell{}).Spec().Description
	for _, part := range []string{"foreground", "no managed background-job API", "shell exit"} {
		if !strings.Contains(description, part) {
			t.Errorf("shell description omits %q", part)
		}
	}
}

type streamingShellStub struct {
	started chan struct{}
	release chan struct{}
}

type admissionShellStub struct {
	foregroundCalls int
	streamingCalls  int
}

func (s *admissionShellStub) Run(ctx context.Context, _, _ string, _ int) (ShellResult, error) {
	s.foregroundCalls++
	if _, ok := ctx.Deadline(); !ok {
		return ShellResult{}, context.Canceled
	}
	return ShellResult{ExitCode: 0, Stdout: "fallback ran\n"}, nil
}

func (s *admissionShellStub) RunStreaming(context.Context, string, string, int, func(string, []byte)) (ShellResult, error) {
	s.streamingCalls++
	return ShellResult{}, nil
}

func TestShellFallsBackToDeadlineForegroundWhenJobRegistryIsFull(t *testing.T) {
	r, err := jobs.New(context.Background(), 1, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	started := make(chan struct{})
	_, err = r.Start("bash", "occupy slot", func(ctx context.Context, _ func([]byte)) (int, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	stub := &admissionShellStub{}
	shell := Shell{Executor: stub, Jobs: r}
	result, err := shell.Execute(context.Background(), json.RawMessage(`{"command":"pwd","timeout_seconds":3}`))
	if err != nil || !strings.Contains(result, "fallback ran") || stub.foregroundCalls != 1 || stub.streamingCalls != 0 {
		t.Fatalf("foreground fallback = %q, %v; calls = %d/%d", result, err, stub.foregroundCalls, stub.streamingCalls)
	}
	if _, err := shell.Execute(context.Background(), json.RawMessage(`{"command":"pwd","run_in_background":true}`)); !errors.Is(err, jobs.ErrCapacity) {
		t.Fatalf("background admission error = %v, want capacity", err)
	}
	if stub.foregroundCalls != 1 || stub.streamingCalls != 0 {
		t.Fatalf("rejected background command ran: calls = %d/%d", stub.foregroundCalls, stub.streamingCalls)
	}
}

func (s streamingShellStub) Run(ctx context.Context, command, workdir string, limit int) (ShellResult, error) {
	return s.RunStreaming(ctx, command, workdir, limit, nil)
}
func (s streamingShellStub) RunStreaming(ctx context.Context, _ string, _ string, _ int, emit func(string, []byte)) (ShellResult, error) {
	if emit != nil {
		emit("stdout", []byte("first\n"))
	}
	close(s.started)
	select {
	case <-s.release:
		if emit != nil {
			emit("stderr", []byte("second\n"))
		}
		return ShellResult{ExitCode: 7, Stdout: "first\n", Stderr: "second\n"}, nil
	case <-ctx.Done():
		return ShellResult{}, ctx.Err()
	}
}

func TestManagedShellBackgroundAndJobTools(t *testing.T) {
	r, err := jobs.New(context.Background(), 1, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	stub := streamingShellStub{started: make(chan struct{}), release: make(chan struct{})}
	shell := Shell{Executor: stub, Jobs: r}
	if !strings.Contains(shell.Spec().Description, "run_in_background") {
		t.Fatal("managed shell schema does not expose background mode")
	}
	result, err := shell.Execute(context.Background(), json.RawMessage(`{"command":"test","run_in_background":true}`))
	if err != nil || !strings.Contains(result, "job_id=") {
		t.Fatalf("background command = %q, %v", result, err)
	}
	<-stub.started
	jobID := strings.Fields(result)[0][len("job_id="):]
	jobTool := JobOutput{Registry: r}
	if !strings.Contains(string(jobTool.Spec().Parameters), `"timeout_ms"`) || strings.Contains(string(jobTool.Spec().Parameters), `"timeout_seconds"`) {
		t.Fatalf("job_output schema does not use upstream milliseconds: %s", jobTool.Spec().Parameters)
	}
	if err := jobTool.Validate(json.RawMessage(`{"job_id":"` + jobID + `","timeout_seconds":1}`)); err != nil {
		t.Fatalf("upstream-open job_output root rejected undeclared field: %v", err)
	}
	if err := jobTool.Validate(json.RawMessage(`{"job_id":"` + jobID + `","timeout_ms":-1}`)); err == nil {
		t.Fatal("job_output accepted negative timeout_ms")
	}
	immediateCtx, immediateCancel := context.WithTimeout(context.Background(), time.Second)
	defer immediateCancel()
	immediate, err := jobTool.Execute(immediateCtx, json.RawMessage(`{"job_id":"`+jobID+`","wait":true,"timeout_ms":0}`))
	if err != nil || !strings.Contains(immediate, "stdout: first") || !strings.Contains(immediate, "status=running") {
		t.Fatalf("zero-millisecond wait should return live status immediately: %q, %v", immediate, err)
	}
	output, err := (JobOutput{Registry: r}).Execute(context.Background(), json.RawMessage(`{"job_id":"`+jobID+`"}`))
	if err != nil || !strings.Contains(output, "status=running") {
		t.Fatalf("live job output = %q, %v", output, err)
	}
	close(stub.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := r.Wait(ctx, jobID, time.Second); err != nil {
		t.Fatal(err)
	}
	output, err = (JobOutput{Registry: r}).Execute(context.Background(), json.RawMessage(`{"job_id":"`+jobID+`"}`))
	if err != nil || !strings.Contains(output, "stderr: second") || !strings.Contains(output, "status=completed") {
		t.Fatalf("settled job output = %q, %v", output, err)
	}
}

func TestJobKillForwardsReasonAndDistinguishesFinishedJob(t *testing.T) {
	r, err := jobs.New(context.Background(), 1, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	cause := make(chan error, 1)
	job, err := r.Start("bash", "cancel me", func(ctx context.Context, _ func([]byte)) (int, error) {
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return 0, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.MarkVisible(job.ID); err != nil {
		t.Fatal(err)
	}
	tool := JobKill{Registry: r}
	if !strings.Contains(string(tool.Spec().Parameters), `"reason"`) {
		t.Fatalf("job_kill schema omits reason: %s", tool.Spec().Parameters)
	}
	arguments := json.RawMessage(`{"job_id":"` + job.ID + `","reason":"no longer needed"}`)
	result, err := tool.Execute(context.Background(), arguments)
	if err != nil || !strings.Contains(result, "outcome=cancellation-requested") || !strings.Contains(result, "status=stopping") {
		t.Fatalf("model kill = %q, %v", result, err)
	}
	if received := <-cause; received == nil || received.Error() != "no longer needed" {
		t.Fatalf("Job cancellation cause = %v", received)
	}
	if snapshot, err := r.Wait(context.Background(), job.ID, time.Second); err != nil || snapshot.Status != jobs.Killed || snapshot.Detail != "no longer needed" {
		t.Fatalf("killed Job = %+v, %v", snapshot, err)
	}
	result, err = tool.Execute(context.Background(), arguments)
	if err != nil || !strings.Contains(result, "outcome=already-finished") || !strings.Contains(result, "status=killed") {
		t.Fatalf("repeat kill = %q, %v", result, err)
	}
	if pending := r.PendingSettled(); len(pending) != 0 {
		t.Fatalf("model-killed Job produced a redundant notice: %+v", pending)
	}
}
