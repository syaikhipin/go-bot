package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"go-bot/harness/agent"
)

func exitPlanModeSpec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "exit_plan_mode",
		Description: "Use only in plan mode. Present your complete plan for review. Approval leaves plan mode from the next step; otherwise revise it using the user's feedback.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"plan":{"type":"string","description":"The complete markdown plan, starting with a # heading."}},"required":["plan"]}`),
	}
}

func decodePlan(raw json.RawMessage) (string, error) {
	var args struct {
		Plan string `json:"plan"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&args); err != nil {
		return "", err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return "", errors.New("exit_plan_mode requires one JSON object")
	}
	if !strings.HasPrefix(strings.TrimSpace(args.Plan), "# ") || strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args.Plan), "# ")) == "" {
		return "", errors.New("exit_plan_mode requires a non-empty markdown plan starting with a # heading")
	}
	return args.Plan, nil
}
