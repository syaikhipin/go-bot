package sessionformat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type advertisedTool struct {
	name      string
	arguments string
	started   bool
}

// ValidateCoreRelationships checks the native turn/step and advertised-tool
// lifecycle. Open tails are valid: a resumed Agent closes an interrupted turn.
// Other v4 relationships (compaction, retries, PTC, titles) are separate work;
// this function is not by itself permission to resume an artifact.
func ValidateCoreRelationships(events []Event) error {
	var turn, step uint64
	nextTurn, nextStep := uint64(1), uint64(1)
	tools := make(map[string]advertisedTool)
	for index, event := range events {
		if err := event.ValidateEnvelope(uint64(index)); err != nil {
			return fmt.Errorf("event %d: %w", index, err)
		}
		fail := func(err error) error { return fmt.Errorf("%s at seq %d: %w", event.Type, event.Seq, err) }
		switch event.Type {
		case "turn/start":
			data, err := objectData(event)
			if err != nil {
				return fail(err)
			}
			got, err := safeCount(data["turn"])
			if err != nil || turn != 0 || got != nextTurn {
				return fail(errors.New("does not open the expected turn"))
			}
			turn, nextStep = got, 1
			clear(tools)
		case "turn/end":
			data, err := objectData(event)
			if err != nil {
				return fail(err)
			}
			got, err := safeCount(data["turn"])
			if err != nil || turn == 0 || got != turn || step != 0 || len(tools) != 0 {
				return fail(errors.New("does not close an open turn with settled tools"))
			}
			turn, nextTurn = 0, turn+1
		case "step/start":
			data, err := objectData(event)
			if err != nil {
				return fail(err)
			}
			gotTurn, turnErr := safeCount(data["turn"])
			gotStep, stepErr := safeCount(data["step"])
			if turnErr != nil || stepErr != nil || turn == 0 || step != 0 || gotTurn != turn || gotStep != nextStep {
				return fail(errors.New("does not open the next step"))
			}
			step = gotStep
		case "step/end":
			data, err := objectData(event)
			if err != nil {
				return fail(err)
			}
			if err := requireStep(data, turn, step); err != nil || len(tools) != 0 {
				return fail(errors.New("does not close the open step with settled tools"))
			}
			step, nextStep = 0, step+1
		case "system/message", "developer/message", "assistant/attempt", "assistant/message", "tool/call", "tool/result":
			data, err := objectData(event)
			if err != nil {
				return fail(err)
			}
			if event.Type == "tool/result" && isSurfaceReplace(event.SurfaceOp) {
				if turn == 0 {
					return fail(errors.New("tool/result rewrite is outside an open turn"))
				}
				continue
			}
			// Constructor messages have no model execution lifecycle. Only the
			// initial system + welcome prefix can precede the first real turn.
			if turn == 0 && nextTurn == 1 && (isInitialSystemMessage(event) || (isInitialWelcomeMessage(event) && isInitialSystemMessage(events[0]))) {
				continue
			}
			if err := requireStep(data, turn, step); err != nil {
				return fail(err)
			}
			switch event.Type {
			case "assistant/message":
				message, err := objectValue(data["message"])
				if err != nil {
					return fail(err)
				}
				var blocks []json.RawMessage
				if json.Unmarshal(message["content"], &blocks) != nil || blocks == nil {
					return fail(errors.New("assistant content must be an array"))
				}
				for _, raw := range blocks {
					block, err := objectValue(raw)
					if err != nil {
						return fail(err)
					}
					if !bytes.Equal(block["type"], []byte(`"tool-call"`)) {
						continue
					}
					id, idErr := requiredString(block["id"])
					name, nameErr := requiredString(block["name"])
					arguments, argsErr := stringValue(block["arguments"])
					if idErr != nil || nameErr != nil || argsErr != nil {
						return fail(errors.New("advertised tool call requires id, name, and raw arguments"))
					}
					if _, duplicate := tools[id]; duplicate {
						return fail(fmt.Errorf("repeats advertised tool call %s", id))
					}
					tools[id] = advertisedTool{name: name, arguments: arguments}
				}
			case "tool/call":
				id, idErr := requiredString(data["callId"])
				name, nameErr := requiredString(data["name"])
				arguments, argsErr := stringValue(data["arguments"])
				if idErr != nil || nameErr != nil || argsErr != nil {
					return fail(errors.New("tool/call requires callId, name, and raw arguments"))
				}
				pending, found := tools[id]
				if !found || pending.started || pending.name != name || pending.arguments != arguments {
					return fail(fmt.Errorf("tool/call %s does not match an advertised call", id))
				}
				pending.started = true
				tools[id] = pending
			case "tool/result":
				message, err := objectValue(data["message"])
				if err != nil {
					return fail(err)
				}
				id, err := requiredString(message["toolCallId"])
				if err != nil {
					return fail(err)
				}
				pending, found := tools[id]
				if !found || !pending.started && !notStartedToolRepair(event, data, message, id) {
					return fail(fmt.Errorf("tool/result %s has no started advertised call", id))
				}
				delete(tools, id)
			}
		case "request/header", "request/context":
			if turn == 0 {
				return fail(errors.New("request metadata is outside an open turn"))
			}
		}
	}
	return nil
}

func objectData(event Event) (map[string]json.RawMessage, error) {
	return objectValue(event.Data)
}

func objectValue(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, errors.New("event data must be an object")
	}
	return value, nil
}

func requireStep(data map[string]json.RawMessage, turn, step uint64) error {
	gotTurn, turnErr := safeCount(data["turn"])
	gotStep, stepErr := safeCount(data["step"])
	if turnErr != nil || stepErr != nil || turn == 0 || step == 0 || gotTurn != turn || gotStep != step {
		return errors.New("does not match an open turn and step")
	}
	return nil
}

func stringValue(raw json.RawMessage) (string, error) {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", errors.New("expected string")
	}
	return value, nil
}

func requiredString(raw json.RawMessage) (string, error) {
	value, err := stringValue(raw)
	if err != nil || value == "" {
		return "", errors.New("expected nonempty string")
	}
	return value, nil
}

func isSurfaceReplace(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte(`"append"`))
}

func notStartedToolRepair(event Event, data, message map[string]json.RawMessage, callID string) bool {
	if len(event.SourceEventSeqs) != 0 || !bytes.Equal(message["isError"], []byte("true")) {
		return false
	}
	failure, err := objectValue(data["error"])
	if err != nil || !bytes.Equal(failure["name"], []byte(`"ToolNotStartedError"`)) || !bytes.Equal(failure["code"], []byte(`"TOOL_NOT_STARTED"`)) {
		return false
	}
	id, err := requiredString(message["id"])
	if err != nil {
		return false
	}
	if strings.HasPrefix(id, "forked-tool-result-"+callID+"-") {
		return true // fork identity is validated by its own v4 relationship rule
	}
	prefix := "interrupted-tool-result-" + callID + "-"
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(id, prefix)
	if suffix == "" || len(suffix) > 1 && suffix[0] == '0' {
		return false
	}
	for _, digit := range suffix {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	if _, err := strconv.ParseUint(suffix, 10, 53); err != nil {
		return false
	}
	var blocks []json.RawMessage
	if json.Unmarshal(message["content"], &blocks) != nil || len(blocks) != 1 {
		return false
	}
	block, err := objectValue(blocks[0])
	return err == nil && bytes.Equal(block["type"], []byte(`"text"`)) && bytes.Equal(block["text"], []byte(`"The tool call was interrupted before the Harness recorded it as started. Retry it if it is still needed."`))
}
