package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"go-harness/harness/agent"
	"go-harness/harness/jobs"
)

type JobOutput struct{ Registry *jobs.Registry }
type JobList struct{ Registry *jobs.Registry }
type JobKill struct{ Registry *jobs.Registry }

type jobOutputArgs struct {
	JobID     string   `json:"job_id"`
	Wait      bool     `json:"wait"`
	TimeoutMS *float64 `json:"timeout_ms"`
}

func (JobOutput) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "job_output", Description: "Read new output and status from a managed background Job. wait=true waits up to timeout_ms (default 30000, capped at 600000) without stopping the Job; timeout_ms=0 checks immediately.", Parameters: json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"},"wait":{"type":"boolean"},"timeout_ms":{"type":"number"}},"required":["job_id"]}`), ParallelSafe: true}
}

func (t JobOutput) Validate(raw json.RawMessage) error {
	if t.Registry == nil {
		return errors.New("Job Registry is unavailable")
	}
	args, err := decodeJobOutputArgs(raw)
	if err != nil {
		return err
	}
	if args.JobID == "" || len(args.JobID) > 128 || args.TimeoutMS != nil && (math.IsNaN(*args.TimeoutMS) || math.IsInf(*args.TimeoutMS, 0) || *args.TimeoutMS < 0) {
		return errors.New("job_id and a nonnegative timeout_ms are required")
	}
	return nil
}

func decodeJobOutputArgs(raw json.RawMessage) (jobOutputArgs, error) {
	var args jobOutputArgs
	return args, decodeOneToolObject(raw, &args)
}

func (t JobOutput) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := t.Validate(raw); err != nil {
		return "", err
	}
	args, err := decodeJobOutputArgs(raw)
	if err != nil {
		return "", err
	}
	if args.Wait {
		milliseconds := 30000.0
		if args.TimeoutMS != nil {
			milliseconds = math.Min(*args.TimeoutMS, 600000)
		}
		if _, err := t.Registry.WaitForModel(ctx, args.JobID, time.Duration(milliseconds*float64(time.Millisecond))); err != nil {
			return "", err
		}
	}
	output, err := t.Registry.Consume(args.JobID)
	if err != nil {
		return "", err
	}
	if output.Job.Status == jobs.Completed || output.Job.Status == jobs.Failed || output.Job.Status == jobs.Killed {
		t.Registry.SuppressNotice(args.JobID)
	}
	return fmt.Sprintf("job_id=%s status=%s exit_code=%d lossy=%t next_cursor=%d\n%s", output.Job.ID, output.Job.Status, output.Job.ExitCode, output.Lossy, output.Cursor, output.Text), nil
}

func (JobList) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "job_list", Description: "List this Agent's managed background Jobs and their current status.", Parameters: json.RawMessage(`{"type":"object","properties":{}}`), ParallelSafe: true}
}
func (t JobList) Validate(raw json.RawMessage) error {
	if t.Registry == nil {
		return errors.New("Job Registry is unavailable")
	}
	var args struct{}
	return decodeOneToolObject(raw, &args)
}
func (t JobList) Execute(context.Context, json.RawMessage) (string, error) {
	encoded, err := json.Marshal(t.Registry.List())
	return string(encoded), err
}

type jobKillArgs struct {
	JobID  string `json:"job_id"`
	Reason string `json:"reason"`
}

func (JobKill) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "job_kill", Description: "Request cancellation of a managed background Job. An optional reason is forwarded to the Job.", Parameters: json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"},"reason":{"type":"string"}},"required":["job_id"]}`)}
}
func (t JobKill) Validate(raw json.RawMessage) error {
	if t.Registry == nil {
		return errors.New("Job Registry is unavailable")
	}
	var args jobKillArgs
	if err := decodeOneToolObject(raw, &args); err != nil {
		return err
	}
	if args.JobID == "" || len(args.JobID) > 128 {
		return errors.New("job_id is required")
	}
	return nil
}
func (t JobKill) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	if err := t.Validate(raw); err != nil {
		return "", err
	}
	var args jobKillArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	snapshot, outcome, err := t.Registry.RequestKill(args.JobID, args.Reason)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("outcome=%s job_id=%s status=%s", outcome, snapshot.ID, snapshot.Status), nil
}
