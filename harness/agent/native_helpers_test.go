//go:build !serveragents_v1_legacy

package agent

import (
	"context"
	"encoding/json"
)

type schemaProbeTool struct {
	schema      json.RawMessage
	validations int
	executions  int
}

func (t *schemaProbeTool) Spec() ToolSpec {
	return ToolSpec{Name: "schema-probe", Parameters: t.schema}
}

func (t *schemaProbeTool) Validate(json.RawMessage) error {
	t.validations++
	return nil
}

func (t *schemaProbeTool) Execute(context.Context, json.RawMessage) (string, error) {
	t.executions++
	return "ok", nil
}

func modelImageOffloadFlags(messages []Message) []bool {
	var flags []bool
	for _, message := range messages {
		imageIndex := 0
		for _, block := range message.ContentBlocks {
			if block.Type == "image" {
				flags = append(flags, message.IsImageOffloaded(imageIndex))
				imageIndex++
			}
		}
	}
	return flags
}
