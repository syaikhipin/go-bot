package skills

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type invalidSkill string

func (e invalidSkill) Error() string { return string(e) }

func parseSkill(raw string) (Summary, string, error) {
	first, remaining, found := strings.Cut(raw, "\n")
	if !found || strings.TrimSuffix(first, "\r") != "---" {
		return Summary{}, "", invalidSkill("missing YAML frontmatter")
	}
	start := remaining
	for {
		line, rest, more := strings.Cut(remaining, "\n")
		if strings.TrimSuffix(line, "\r") == "---" {
			return parseFields(start[:len(start)-len(remaining)], strings.TrimFunc(rest, IsSpace))
		}
		if !more {
			return Summary{}, "", invalidSkill("missing closing frontmatter delimiter")
		}
		remaining = rest
	}
}

func parseFields(frontmatter, body string) (Summary, string, error) {
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(frontmatter), &fields); err != nil {
		return Summary{}, "", invalidSkill("invalid YAML frontmatter")
	}
	name, _ := fields["name"].(string)
	description, _ := fields["description"].(string)
	if !IsName(name) || description == "" {
		return Summary{}, "", invalidSkill("frontmatter requires a valid skill name and a description")
	}
	for _, legacy := range []string{"disableModelInvocation", "modelInvocable", "userInvocable"} {
		if _, present := fields[legacy]; present {
			return Summary{}, "", invalidSkill(fmt.Sprintf("unsupported frontmatter field %q", legacy))
		}
	}
	disabled, err := invocationField(fields, "disable-model-invocation", false)
	if err != nil {
		return Summary{}, "", err
	}
	user, err := invocationField(fields, "user-invocable", true)
	if err != nil {
		return Summary{}, "", err
	}
	when, _ := fields["whenToUse"].(string)
	metadata, _ := fields["metadata"].(map[string]any)
	return Summary{
		Name: name, Description: description, WhenToUse: when, Metadata: metadata,
		ModelInvocable: !disabled, UserInvocable: user,
	}, body, nil
}

func invocationField(fields map[string]any, key string, fallback bool) (bool, error) {
	value, present := fields[key]
	if !present {
		return fallback, nil
	}
	switch value := value.(type) {
	case bool:
		return value, nil
	case int:
		if value == 0 || value == 1 {
			return value == 1, nil
		}
	case float64:
		if value == 0 || value == 1 {
			return value == 1, nil
		}
	case string:
		switch strings.ToLower(value) {
		case "1", "true", "yes", "on":
			return true, nil
		case "0", "false", "no", "off":
			return false, nil
		}
	}
	return false, invalidSkill(fmt.Sprintf("frontmatter field %q must be a boolean", key))
}

var textEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func EscapeText(value string) string { return textEscaper.Replace(value) }
