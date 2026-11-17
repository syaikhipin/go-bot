package tools

import (
	"context"
	"encoding/json"
	"errors"

	"go-bot/harness/agent"
	"go-bot/harness/memory"
)

type NativeMemory struct{ Store *memory.Store }

func (t NativeMemory) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "memory", Description: "Save, correct, or forget durable user preferences and reusable facts for this account. Use user for preferences and memory for verified notes. The next model step loads the saved changes.",
		Parameters: json.RawMessage(`{"type":"object","required":["target","operations"],"additionalProperties":false,"properties":{
			"target":{"type":"string","enum":["user","memory"]},
			"operations":{"type":"array","minItems":1,"maxItems":20,"items":{"type":"object","required":["action"],"additionalProperties":false,"properties":{
				"action":{"type":"string","enum":["add","patch","remove"]},
				"old_text":{"type":"string","description":"Exact substring identifying one existing entry for patch or remove."},
				"content":{"type":"string","description":"Entry to add, or text replacing the matched substring for patch."}
			}}}
		}}`),
	}
}

type memoryArgs struct {
	Target     string             `json:"target"`
	Operations []memory.Operation `json:"operations"`
}

func decodeMemory(raw json.RawMessage) (memoryArgs, error) {
	var args memoryArgs
	if err := decodeStrictToolObject(raw, &args); err != nil {
		return args, err
	}
	if args.Target != "user" && args.Target != "memory" {
		return args, errors.New("memory target must be user or memory")
	}
	return args, memory.ValidateOperations(args.Operations)
}

func (t NativeMemory) Validate(raw json.RawMessage) error {
	_, err := decodeMemory(raw)
	return err
}

func (t NativeMemory) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	args, err := decodeMemory(raw)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	result, err := t.Store.Apply(args.Target, args.Operations)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(result)
	return string(out), err
}
