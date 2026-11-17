package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go-bot/harness/agent"
	"go-bot/harness/skills"
)

type Skill struct {
	Registry *skills.Registry
	Lookup   func() skills.Lookup
}

func (t Skill) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "skill",
		Description: "Load the full instructions for a skill. Call it before acting on a task that names or clearly matches a skill in the session skill catalog.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"The exact skill name from the available skills list."}},"required":["name"]}`),
	}
}

type skillArgs struct {
	Name string `json:"name"`
}

// The agent validates the argument schema before execution.
func (t Skill) Validate(json.RawMessage) error { return nil }

func (t Skill) ExecuteValue(ctx context.Context, raw json.RawMessage) (skills.Value, error) {
	var args skillArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return skills.Value{}, err
	}
	if !skills.IsName(args.Name) {
		return skills.Value{}, fmt.Errorf("invalid skill name \"%s\"", args.Name)
	}
	candidate, found, err := t.Registry.Find(ctx, t.Lookup(), args.Name)
	if err != nil {
		return skills.Value{}, err
	}
	unknown := fmt.Errorf("skill \"%s\" is unknown or no longer available", args.Name)
	disabled := fmt.Errorf("skill \"%s\" is not available for model invocation", args.Name)
	if !found {
		return skills.Value{}, unknown
	}
	if !candidate.ModelInvocable {
		return skills.Value{}, disabled
	}
	loaded, found, err := candidate.Load(ctx)
	if err != nil {
		return skills.Value{}, err
	}
	if !found {
		return skills.Value{}, unknown
	}
	if !loaded.ModelInvocable {
		return skills.Value{}, disabled
	}
	return loaded.Value, nil
}

func (t Skill) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	value, err := t.ExecuteValue(ctx, raw)
	if err != nil {
		return "", err
	}
	return RenderSkillContent(value), nil
}

func (t Skill) ExecutePresented(ctx context.Context, raw json.RawMessage) (string, json.RawMessage, error) {
	text, err := t.Execute(ctx, raw)
	return text, nil, err
}

var skillAttribute = strings.NewReplacer("&", "&amp;", "\"", "&quot;", "<", "&lt;")

func RenderSkillContent(value skills.Value) string {
	resources := fmt.Sprintf("Resources for this skill are managed by provider \"%s\".\nLoad referenced resources only as needed.", skills.EscapeText(value.Provider))
	if value.ResourceBase != nil {
		resources = fmt.Sprintf("Base directory for this skill: %s\nResolve relative paths mentioned by this skill against the base directory before using them. Load referenced resources only as needed.", skills.EscapeText(value.ResourceBase.Path))
	}
	return fmt.Sprintf("<skill_content name=\"%s\">\n<skill_resources>\n%s\n</skill_resources>\n\n<skill_instructions>\n%s\n</skill_instructions>\n</skill_content>",
		skillAttribute.Replace(value.Name), resources, value.Content)
}
