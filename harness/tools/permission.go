package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"go-bot/harness/agent"
	"go-bot/harness/permission"
)

type PermissionTool struct {
	Tool            agent.Tool
	State           func() permission.State
	Root            string
	Approvals       permission.ApprovalService
	ConfineMutation func(context.Context) context.Context
}

func (t PermissionTool) Spec() agent.ToolSpec {
	spec := t.Tool.Spec()
	var schema struct {
		Type                 string         `json:"type"`
		Properties           map[string]any `json:"properties"`
		Required             []string       `json:"required"`
		AdditionalProperties bool           `json:"additionalProperties"`
	}
	if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
		panic(err)
	}
	subject := t.subject()
	schema.Properties["sandbox_permissions"] = map[string]any{
		"type": "string", "enum": []string{"workspace-write", "danger-full-access"},
		"description": fmt.Sprintf("The narrowest wider sandbox mode for a one-shot retry of the exact %s the sandbox just denied; the retry asks the user for approval.", subject),
	}
	schema.Properties["justification"] = map[string]any{
		"type": "string", "description": "Explain in one sentence why this action needs wider sandbox permissions, using the user's language.",
	}
	spec.Parameters, _ = json.Marshal(schema)
	if spec.Name == "shellexecute" {
		spec.Description = "Run a shell command in the Host using the current session file sandbox. Normally omit sandbox_permissions. If the sandbox denies a necessary action, follow the returned guidance and request only the narrowest wider mode. Set run_in_background to start a managed Job; a foreground command exceeding the wait timeout can also become a Job. Use job_output, job_list, and job_kill for managed Jobs."
	}
	return spec
}

func (t PermissionTool) subject() string {
	if t.Tool.Spec().Name == "shellexecute" {
		return "command"
	}
	return "operation"
}

type escalationArgs struct {
	original       json.RawMessage
	mode           permission.Mode
	reason, detail string
}

func decodeEscalation(raw json.RawMessage) (escalationArgs, error) {
	var fields map[string]json.RawMessage
	var args escalationArgs
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return args, errors.New("tool arguments must be an object")
	}
	for _, field := range []struct {
		name   string
		target any
	}{{"sandbox_permissions", &args.mode}, {"justification", &args.reason}} {
		if value, ok := fields[field.name]; ok {
			if err := json.Unmarshal(value, field.target); err != nil {
				return args, err
			}
			delete(fields, field.name)
		}
	}
	args.reason = strings.TrimSpace(args.reason)
	if len(args.reason) > 1000 {
		return args, errors.New("justification exceeds 1000 bytes")
	}
	if args.mode == "" && args.reason != "" || args.mode != "" && args.mode != permission.WorkspaceWrite && args.mode != permission.FullAccess {
		return args, errors.New("invalid sandbox_permissions and justification")
	}
	var command, cwd, path string
	_ = json.Unmarshal(fields["command"], &command)
	_ = json.Unmarshal(fields["workdir"], &cwd)
	_ = json.Unmarshal(fields["path"], &path)
	if command != "" {
		args.detail = command
		if cwd != "" {
			args.detail += "\nWorking directory: " + cwd
		}
	} else {
		args.detail = path
	}
	args.original, _ = json.Marshal(fields)
	return args, nil
}

func (t PermissionTool) Validate(raw json.RawMessage) error {
	args, err := decodeEscalation(raw)
	if err != nil {
		return err
	}
	return t.Tool.Validate(args.original)
}

func (t PermissionTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	args, err := decodeEscalation(raw)
	if err != nil {
		return "", err
	}
	state := t.State()
	mode := state.Mode
	if args.mode != "" && args.mode != mode {
		if args.mode.Rank() <= mode.Rank() || args.reason == "" {
			return "", errors.New("request a strictly wider sandbox mode with a justification")
		}
		err := t.Approvals.Request(ctx, permission.Request{Tool: t.Tool.Spec().Name, Reason: args.reason, Detail: args.detail, Target: args.mode})
		if err != nil {
			return "", err
		}
		mode = args.mode
	}
	ctx = permission.WithScope(ctx, permission.Scope{Mode: mode, Root: t.Root, Ask: state.Approval == "ask"})
	run := func() (string, error) { return t.Tool.Execute(ctx, args.original) }
	if t.subject() == "command" || mode == permission.FullAccess {
		return run()
	}
	if mode == permission.ReadOnly {
		return "", permission.Failure{Code: "FS_SANDBOX_DENIED", Message: permission.Denial(mode, state.Approval == "ask", "operation")}
	}
	if t.ConfineMutation == nil {
		return "", permission.Failure{Code: "SANDBOX_UNAVAILABLE", Message: "Linux file sandbox unavailable"}
	}
	content, err := t.Tool.Execute(t.ConfineMutation(ctx), args.original)
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EXDEV) {
		return "", permission.Failure{Code: "FS_SANDBOX_DENIED", Message: permission.Denial(mode, state.Approval == "ask", "operation")}
	}
	return content, err
}
