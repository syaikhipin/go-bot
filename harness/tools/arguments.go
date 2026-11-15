package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// decodeOneToolObject accepts the open parameter root produced by DeepSeek's
// defineTool while preventing a valid prefix from being executed when trailing
// data remains. Mutating tools must reject the entire call before side effects.
func decodeOneToolObject(raw json.RawMessage, into any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("tool arguments must be one JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("tool arguments must be one JSON object")
	}
	return nil
}

// decodeStrictToolObject is for our file-tool dialect, whose parameter roots
// explicitly reject undeclared arguments (such as obsolete byte offsets).
func decodeStrictToolObject(raw json.RawMessage, into any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("tool arguments must be one JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("tool arguments must be one JSON object")
	}
	return nil
}

// decodeGoalArgs is shared by native Goal and Subagent tools. Their argument
// roots are open, but each call must still contain exactly one JSON object.
func decodeGoalArgs(raw json.RawMessage, into any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("goal tool requires one JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("goal tool requires one JSON object")
	}
	return nil
}

// projectToolRoot ignores undeclared properties of an open tool-parameter
// root, while allowing a downstream decoder to keep nested objects strict.
// The returned fields contain only properties declared by that tool.
func projectToolRoot(raw json.RawMessage, names ...string) (json.RawMessage, map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := decodeOneToolObject(raw, &fields); err != nil {
		return nil, nil, err
	}
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		allowed[name] = struct{}{}
	}
	for name := range fields {
		if _, ok := allowed[name]; !ok {
			delete(fields, name)
		}
	}
	projected, err := json.Marshal(fields)
	return projected, fields, err
}
