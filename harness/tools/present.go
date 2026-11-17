package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go-bot/harness/agent"
	"go-bot/harness/session"
	"go-bot/internal/agentfs"
)

type Present struct {
	Workspace string
	CWD       func() string
}

type presentArgs struct {
	Files []sessionformat.PresentedFile `json:"files"`
}

func (Present) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "present",
		Description: "Declare existing final files in the session workspace for the user to download. The user receives the current files at their source paths.",
		Parameters:  json.RawMessage(`{"type":"object","required":["files"],"properties":{"files":{"type":"array","minItems":1,"maxItems":8,"items":{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"description":{"type":"string","maxLength":1024}}}}}}`),
	}
}

func (Present) Validate(raw json.RawMessage) error {
	var args presentArgs
	if err := decodeOneToolObject(raw, &args); err != nil {
		return err
	}
	if len(args.Files) < 1 || len(args.Files) > 8 {
		return fmt.Errorf("present requires 1 to 8 files")
	}
	for _, file := range args.Files {
		if strings.TrimSpace(file.Path) == "" || len(file.Path) > 4096 || strings.ContainsRune(file.Path, 0) || len(file.Description) > 4096 {
			return fmt.Errorf("invalid delivery path or description")
		}
	}
	return nil
}

func (t Present) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	text, _, err := t.ExecutePresented(ctx, raw)
	return text, err
}

func (t Present) ExecutePresented(ctx context.Context, raw json.RawMessage) (string, json.RawMessage, error) {
	if err := t.Validate(raw); err != nil {
		return "", nil, err
	}
	var args presentArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", nil, err
	}
	cwd := t.CWD()
	lines := make([]string, 0, len(args.Files))
	for i, entry := range args.Files {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		path, rel, err := agentfs.PresentedPath(t.Workspace, cwd, entry.Path)
		if err != nil {
			return "", nil, fmt.Errorf("cannot present %q: path must be inside the session workspace", entry.Path)
		}
		file, _, err := agentfs.OpenPresented(t.Workspace, rel)
		if err != nil {
			return "", nil, fmt.Errorf("cannot present %q: %w", entry.Path, err)
		}
		file.Close()
		args.Files[i].Path = path
		lines = append(lines, "Presented "+path)
	}
	meta, err := json.Marshal(args)
	return strings.Join(lines, "\n"), meta, err
}
