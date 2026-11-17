package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"

	"go-bot/internal/modeldata"
)

// RepeatReminderConfig follows DeepSeek Harness's repeat-tool-reminder: exact
// consecutive calls draw model-facing notices, never a tool veto. An excluded
// call is transparent to the chain. Nil config enables the default thresholds.
type RepeatReminderConfig struct {
	Disabled              bool
	Thresholds            []int
	Include               []string
	Exclude               []string
	ArgumentsPreviewChars int
}

type repeatToolReminder struct {
	disabled   bool
	thresholds map[int]bool
	first      int
	include    []*regexp.Regexp
	exclude    []*regexp.Regexp
	preview    int
	key        string
	count      int
}

func newRepeatToolReminder(config *RepeatReminderConfig) (repeatToolReminder, error) {
	guard := repeatToolReminder{thresholds: make(map[int]bool), preview: 500}
	if config != nil && config.Disabled {
		guard.disabled = true
		return guard, nil
	}
	thresholds := []int{3, 5, 8}
	if config != nil {
		if config.Thresholds != nil {
			thresholds = config.Thresholds
		}
		if config.ArgumentsPreviewChars != 0 {
			guard.preview = config.ArgumentsPreviewChars
		}
		var err error
		guard.include, err = compileToolPatterns(config.Include)
		if err != nil {
			return repeatToolReminder{}, err
		}
		guard.exclude, err = compileToolPatterns(config.Exclude)
		if err != nil {
			return repeatToolReminder{}, err
		}
	}
	if len(thresholds) == 0 || guard.preview < 1 {
		return repeatToolReminder{}, errors.New("repeat-tool reminder requires thresholds and a positive argument preview limit")
	}
	for _, threshold := range thresholds {
		if threshold < 2 || guard.thresholds[threshold] {
			return repeatToolReminder{}, fmt.Errorf("invalid or duplicate repeat-tool threshold %d", threshold)
		}
		guard.thresholds[threshold] = true
		if guard.first == 0 || threshold < guard.first {
			guard.first = threshold
		}
	}
	return guard, nil
}

func compileToolPatterns(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		var parts []string
		for _, part := range strings.Split(pattern, "*") {
			parts = append(parts, regexp.QuoteMeta(part))
		}
		compiled = append(compiled, regexp.MustCompile("^"+strings.Join(parts, ".*")+"$"))
	}
	return compiled, nil
}

func (g *repeatToolReminder) reset() {
	g.key, g.count = "", 0
}

func (g *repeatToolReminder) tracked(name string) bool {
	if g.disabled {
		return false
	}
	if len(g.include) > 0 {
		matched := false
		for _, pattern := range g.include {
			matched = matched || pattern.MatchString(name)
		}
		if !matched {
			return false
		}
	}
	for _, pattern := range g.exclude {
		if pattern.MatchString(name) {
			return false
		}
	}
	return true
}

func canonicalToolArguments(raw string) string {
	if raw == "" {
		return `{}`
	}
	if !json.Valid([]byte(raw)) {
		return jsonStringWithoutHTMLEscape(raw)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	if err := decoder.Decode(&value); err != nil {
		return jsonStringWithoutHTMLEscape(raw)
	}
	var encoded bytes.Buffer
	writer := json.NewEncoder(&encoded)
	writer.SetEscapeHTML(false) // JSON.stringify does not escape <, >, or &.
	if err := writer.Encode(value); err != nil {
		return jsonStringWithoutHTMLEscape(raw)
	}
	return strings.TrimSuffix(encoded.String(), "\n")
}

func jsonStringWithoutHTMLEscape(value string) string {
	var encoded bytes.Buffer
	writer := json.NewEncoder(&encoded)
	writer.SetEscapeHTML(false)
	_ = writer.Encode(value)
	return strings.TrimSuffix(encoded.String(), "\n")
}

func (g *repeatToolReminder) observe(call modeldata.ToolRequest) *Message {
	if !g.tracked(call.Name) {
		return nil
	}
	canonical := canonicalToolArguments(call.Arguments)
	encodedKey, _ := json.Marshal([2]string{call.Name, canonical})
	key := string(encodedKey)
	if g.key == key {
		g.count++
	} else {
		g.key, g.count = key, 1
	}
	if !g.thresholds[g.count] {
		return nil
	}
	var content string
	if g.count == g.first {
		content = "You are repeating the exact same tool call with identical arguments. Carefully analyze the previous result before calling again: if the task is not complete, try a different approach or different arguments instead of repeating the call."
	} else {
		preview := canonical
		characters := utf16.Encode([]rune(canonical))
		if len(characters) > g.preview {
			preview = fmt.Sprintf("%s… (+%d more chars)", string(utf16.Decode(characters[:g.preview])), len(characters)-g.preview)
		}
		content = fmt.Sprintf("Repeated tool call detected:\n- tool: %s\n- consecutive_calls: %d\n- arguments: %s\nThe repeated calls are not making progress. Do not call this tool with these exact arguments again. Inspect the latest result and choose a different action, different arguments, or finish the task if enough evidence has been gathered.", call.Name, g.count, preview)
	}
	return &Message{Role: "user", Content: content, Source: &MessageSource{Kind: "repeat-tool-reminder", ID: fmt.Sprintf("%s × %d", call.Name, g.count)}}
}
