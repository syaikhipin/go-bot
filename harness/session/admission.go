package sessionformat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ValidateV4Rows admits released native v4 message and request syntax before
// relationship replay. Unknown ignorable records remain opaque.
func ValidateV4Rows(events []Event) error {
	for _, event := range events {
		if !knownV4EventTypes[event.Type] {
			if !event.Ignorable {
				return fmt.Errorf("unknown required v4 event %q", event.Type)
			}
			continue
		}
		if err := validateV4Row(event); err != nil {
			return fmt.Errorf("%s at seq %d: %w", event.Type, event.Seq, err)
		}
	}
	return nil
}

func validateV4Row(event Event) error {
	data, dataErr := objectValue(event.Data)
	if event.Type == "request/header" {
		if dataErr != nil {
			return errors.New("request/header requires object data")
		}
		header, err := objectValue(data["header"])
		if err != nil {
			return errors.New("request/header requires header object")
		}
		if _, retired := header["system"]; retired {
			return errors.New("request/header uses retired header.system")
		}
		if raw, present := header["tools"]; present {
			var tools []json.RawMessage
			if json.Unmarshal(raw, &tools) == nil {
				if tools != nil && len(tools) == 0 {
					return errors.New("request/header must omit empty tools")
				}
				for _, tool := range tools {
					definition, err := objectValue(tool)
					if err == nil {
						if deferLoading, present := definition["deferLoading"]; present && !bytes.Equal(deferLoading, []byte("true")) {
							return errors.New("tool deferLoading must be true when present")
						}
					}
				}
			}
		}
		if raw, present := header["adapterDefaults"]; present {
			if defaults, err := objectValue(raw); err == nil && len(defaults) == 0 {
				return errors.New("request/header must omit empty adapterDefaults")
			}
		}
	}
	if dataErr != nil {
		if event.Type == "plan/mode" {
			return errors.New("plan/mode requires object data")
		}
		if event.Type == "goal/change" {
			return errors.New("goal/change requires object data")
		}
		if event.Type == "todo/write" {
			return errors.New("todo/write requires object data")
		}
		if event.Type == "developer/message" || event.Type == "system/message" || event.Type == "tool/result" {
			return fmt.Errorf("%s requires object data", event.Type)
		}
		return nil
	}
	if event.Type == "goal/change" {
		if _, err := DecodeGoalChange(event.Data); err != nil {
			return err
		}
	}
	if event.Type == "plan/mode" {
		if _, err := DecodePlanMode(event.Data); err != nil {
			return err
		}
	}
	if event.Type == "todo/write" {
		if _, err := DecodeTodoSnapshot(event.Data, true); err != nil {
			return err
		}
	}
	if err := validateDeclaredMessages(event, data); err != nil {
		return err
	}
	if err := validateRetiredContent(event, data); err != nil {
		return err
	}
	if event.Type == "system/message" {
		if err := validateSystemMessage(data, isInitialSystemMessage(event)); err != nil {
			return err
		}
	}
	if event.Type == "tool/result" {
		if err := validateToolResult(event, data); err != nil {
			return err
		}
	}
	if event.Type == "developer/message" {
		if err := validateDeveloperMessage(data); err != nil {
			return err
		}
	}
	if event.Type == "assistant/message" || event.Type == "assistant/attempt" {
		if err := validateStreamSyntax(data); err != nil {
			return err
		}
	}
	return nil
}

func declaredMessages(event Event, data map[string]json.RawMessage) ([]map[string]json.RawMessage, error) {
	switch event.Type {
	case "user/message":
		return []map[string]json.RawMessage{data}, nil
	case "developer/message", "system/message", "assistant/message", "tool/result":
		message, err := objectValue(data["message"])
		if err != nil {
			return nil, errors.New("message event requires message object")
		}
		return []map[string]json.RawMessage{message}, nil
	case "agent/inbox/spliced", "session/title-llm-request":
		field := "inserted"
		if event.Type == "session/title-llm-request" {
			field = "messages"
		}
		var raw []json.RawMessage
		if json.Unmarshal(data[field], &raw) != nil || raw == nil {
			return nil, fmt.Errorf("%s requires %s array", event.Type, field)
		}
		messages := make([]map[string]json.RawMessage, 0, len(raw))
		for _, item := range raw {
			message, err := objectValue(item)
			if err != nil {
				return nil, fmt.Errorf("%s requires message objects", event.Type)
			}
			messages = append(messages, message)
		}
		return messages, nil
	default:
		return nil, nil
	}
}

func validateDeclaredMessages(event Event, data map[string]json.RawMessage) error {
	messages, err := declaredMessages(event, data)
	if err != nil {
		return err
	}
	for _, message := range messages {
		source, err := objectValue(message["source"])
		if err != nil {
			return errors.New("v4 message requires producer-owned source")
		}
		kind, err := requiredString(source["kind"])
		if err != nil || kind == "plugin" {
			return errors.New("v4 message requires producer-owned source kind")
		}
		if event.Type == "developer/message" {
			if !bytes.Equal(message["role"], []byte(`"developer"`)) {
				return errors.New("developer/message requires developer role")
			}
		} else if bytes.Equal(message["role"], []byte(`"developer"`)) {
			return errors.New("developer role requires developer/message")
		}
		if err := validateToolChanges(message["content"], event.Type == "developer/message"); err != nil {
			return err
		}
	}
	return nil
}

func validateToolChanges(content json.RawMessage, developer bool) error {
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return nil // ordinary incomplete messages are rejected by the Session reader
	}
	for _, raw := range blocks {
		block, err := objectValue(raw)
		if err != nil {
			continue
		}
		addition := bytes.Equal(block["type"], []byte(`"tool-addition"`))
		removal := bytes.Equal(block["type"], []byte(`"tool-removal"`))
		if !addition && !removal {
			continue
		}
		if !developer {
			return errors.New("tool-change blocks require developer role")
		}
		if _, err := requiredString(block["toolName"]); err != nil {
			return errors.New("tool-change block requires toolName")
		}
		if addition {
			if _, present := block["tool"]; present {
				return errors.New("tool-addition must omit inline tool definition")
			}
		}
	}
	return nil
}

func validateRetiredContent(event Event, data map[string]json.RawMessage) error {
	contents := make([]json.RawMessage, 0, 3)
	switch event.Type {
	case "user/message":
		contents = append(contents, data["content"])
	case "developer/message", "assistant/message", "team/message/queued":
		if message, err := objectValue(data["message"]); err == nil {
			contents = append(contents, message["content"])
		}
	case "agent/inbox/spliced", "session/title-llm-request":
		field := "inserted"
		if event.Type == "session/title-llm-request" {
			field = "messages"
		}
		var messages []json.RawMessage
		if json.Unmarshal(data[field], &messages) == nil {
			for _, raw := range messages {
				if message, err := objectValue(raw); err == nil {
					contents = append(contents, message["content"])
				}
			}
		}
	case "compaction/summary":
		contents = append(contents, data["summary"], data["rawOutput"])
	case "tool/ptc-dispatch":
		contents = append(contents, data["content"])
	}
	for _, content := range contents {
		var blocks []json.RawMessage
		if json.Unmarshal(content, &blocks) != nil {
			continue
		}
		for _, raw := range blocks {
			block, err := objectValue(raw)
			if err == nil && bytes.Equal(block["type"], []byte(`"tool-result"`)) {
				return errors.New("v4 content contains retired tool-result wrapper")
			}
		}
	}
	return nil
}

func validateSystemMessage(data map[string]json.RawMessage, constructor bool) error {
	if !constructor {
		for _, field := range []string{"turn", "step"} {
			value, err := safeCount(data[field])
			if err != nil || value == 0 {
				return fmt.Errorf("system/message %s must be positive", field)
			}
		}
	}
	message, err := objectValue(data["message"])
	if err != nil {
		return errors.New("system/message requires message")
	}
	if _, err := requiredString(message["id"]); err != nil || !bytes.Equal(message["role"], []byte(`"system"`)) {
		return errors.New("system/message requires id and system role")
	}
	var blocks []json.RawMessage
	if json.Unmarshal(message["content"], &blocks) != nil || blocks == nil {
		return errors.New("system/message requires content array")
	}
	for _, raw := range blocks {
		block, err := objectValue(raw)
		if err != nil {
			return errors.New("system content block must be an object")
		}
		blockType, err := requiredString(block["type"])
		if err != nil {
			return errors.New("system content block requires type")
		}
		switch blockType {
		case "text", "reasoning":
			if _, err := stringValue(block["text"]); err != nil {
				return errors.New("system text block requires text")
			}
		case "tool-call":
			if _, err := requiredString(block["id"]); err != nil {
				return errors.New("system tool-call requires id")
			}
			if _, err := requiredString(block["name"]); err != nil {
				return errors.New("system tool-call requires name")
			}
			if _, err := stringValue(block["arguments"]); err != nil {
				return errors.New("system tool-call requires arguments")
			}
		case "image":
			if err := validateSystemImage(block["attachment"]); err != nil {
				return err
			}
		case "tool-result":
			return errors.New("system content contains retired tool-result wrapper")
		}
	}
	return nil
}

func validateSystemImage(raw json.RawMessage) error {
	attachment, err := objectValue(raw)
	if err != nil {
		return errors.New("system image requires attachment")
	}
	if _, err := requiredString(attachment["attachmentId"]); err != nil {
		return errors.New("system image requires attachmentId")
	}
	mediaType, err := stringValue(attachment["mediaType"])
	if err != nil || mediaType != "image/png" && mediaType != "image/jpeg" && mediaType != "image/webp" && mediaType != "image/gif" {
		return errors.New("system image has unsupported mediaType")
	}
	if _, err := safeCount(attachment["bytes"]); err != nil {
		return errors.New("system image requires bytes")
	}
	for _, field := range []string{"width", "height"} {
		value, err := safeCount(attachment[field])
		if err != nil || value == 0 {
			return fmt.Errorf("system image %s must be positive", field)
		}
	}
	if raw, present := attachment["name"]; present {
		if _, err := stringValue(raw); err != nil {
			return errors.New("system image name must be a string")
		}
	}
	if raw, present := attachment["originalDimensions"]; present {
		original, err := objectValue(raw)
		if err != nil {
			return errors.New("system originalDimensions must be an object")
		}
		for _, field := range []string{"width", "height"} {
			value, err := safeCount(original[field])
			if err != nil || value == 0 {
				return fmt.Errorf("system original %s must be positive", field)
			}
		}
	}
	return nil
}

func validateToolResult(event Event, data map[string]json.RawMessage) error {
	message, err := objectValue(data["message"])
	if err != nil {
		return errors.New("tool/result requires message")
	}
	if _, err := requiredString(message["id"]); err != nil || !bytes.Equal(message["role"], []byte(`"tool"`)) {
		return errors.New("tool/result requires id and tool role")
	}
	callID, err := requiredString(message["toolCallId"])
	if err != nil {
		return errors.New("tool/result requires toolCallId")
	}
	source, err := objectValue(message["source"])
	if err != nil || !bytes.Equal(source["kind"], []byte(`"tool"`)) || !equalJSON(source["callId"], message["toolCallId"]) {
		return errors.New("tool/result requires matching tool source")
	}
	var blocks []json.RawMessage
	if json.Unmarshal(message["content"], &blocks) != nil || blocks == nil {
		return errors.New("tool/result requires array content")
	}
	for _, raw := range blocks {
		block, err := objectValue(raw)
		if err == nil && bytes.Equal(block["type"], []byte(`"tool-result"`)) {
			return errors.New("tool/result content contains retired wrapper")
		}
	}
	if value, present := message["isError"]; present && !bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false")) {
		return errors.New("tool/result isError must be boolean")
	}
	if _, present := data["error"]; present && !bytes.Equal(message["isError"], []byte("true")) {
		return errors.New("tool/result error requires message.isError")
	}
	return validateForkResult(event, data, message, source, callID)
}

func validateForkResult(event Event, data, message, source map[string]json.RawMessage, callID string) error {
	failure, err := objectValue(data["error"])
	if err != nil || !bytes.Equal(failure["code"], []byte(`"TOOL_NOT_STARTED"`)) {
		return nil
	}
	id, err := stringValue(message["id"])
	if err != nil || !strings.HasPrefix(id, "forked-tool-result-") {
		return nil
	}
	prefix := "forked-tool-result-" + callID + "-"
	if !strings.HasPrefix(id, prefix) {
		return errors.New("invalid v4 fork result identity")
	}
	suffix := strings.TrimPrefix(id, prefix)
	if suffix == "" || len(suffix) > 1 && suffix[0] == '0' {
		return errors.New("invalid v4 fork result sequence")
	}
	for _, digit := range suffix {
		if digit < '0' || digit > '9' {
			return errors.New("invalid v4 fork result sequence")
		}
	}
	sequence, err := strconv.ParseUint(suffix, 10, 53)
	if err != nil {
		return errors.New("invalid v4 fork result sequence")
	}
	replacement := isSurfaceReplace(event.SurfaceOp)
	if replacement && sequence >= event.Seq || !replacement && sequence != event.Seq {
		return errors.New("invalid v4 fork result event sequence")
	}
	if replacement {
		sources, err := expandSourceSeqs(event.SourceEventSeqs)
		if err != nil || len(sources) != 1 {
			return errors.New("fork result replacement requires one source")
		}
		if _, found := sources[sequence]; !found {
			return errors.New("fork result replacement cites wrong source")
		}
	} else if len(event.SourceEventSeqs) != 0 {
		return errors.New("append fork result cannot cite source events")
	}
	if !bytes.Equal(failure["name"], []byte(`"ToolNotStartedError"`)) || !bytes.Equal(message["isError"], []byte("true")) || !bytes.Equal(source["kind"], []byte(`"tool"`)) {
		return errors.New("fork result has invalid error or source")
	}
	var blocks []json.RawMessage
	if json.Unmarshal(message["content"], &blocks) != nil || len(blocks) != 1 {
		return errors.New("fork result requires one text block")
	}
	block, err := objectValue(blocks[0])
	if err != nil || !bytes.Equal(block["type"], []byte(`"text"`)) {
		return errors.New("fork result requires one text block")
	}
	if _, err := stringValue(block["text"]); err != nil {
		return errors.New("fork result requires text")
	}
	return nil
}

func validateDeveloperMessage(data map[string]json.RawMessage) error {
	for _, field := range []string{"turn", "step"} {
		value, err := safeCount(data[field])
		if err != nil || value == 0 {
			return fmt.Errorf("developer/message %s must be positive", field)
		}
	}
	message, err := objectValue(data["message"])
	if err != nil || !bytes.Equal(message["role"], []byte(`"developer"`)) {
		return errors.New("developer/message requires developer role")
	}
	if _, err := requiredString(message["id"]); err != nil {
		return errors.New("developer/message requires id")
	}
	var blocks []json.RawMessage
	if json.Unmarshal(message["content"], &blocks) != nil || blocks == nil {
		return errors.New("developer/message requires content array")
	}
	additions := false
	for _, raw := range blocks {
		block, err := objectValue(raw)
		if err == nil && bytes.Equal(block["type"], []byte(`"tool-addition"`)) {
			additions = true
		}
	}
	_, hasHeader := data["headerSeq"]
	if additions != hasHeader {
		return errors.New("developer/message requires headerSeq exactly for tool additions")
	}
	if hasHeader {
		if _, err := safeCount(data["headerSeq"]); err != nil {
			return errors.New("developer/message headerSeq must be a safe count")
		}
	}
	return nil
}

func validateStreamSyntax(data map[string]json.RawMessage) error {
	var entries []json.RawMessage
	if json.Unmarshal(data["stream"], &entries) != nil {
		return nil // incomplete ordinary streams belong to Session payload admission
	}
	for _, raw := range entries {
		entry, err := objectValue(raw)
		if err != nil || !bytes.Equal(entry["type"], []byte(`"chunk"`)) {
			continue
		}
		chunk, err := objectValue(entry["chunk"])
		if err != nil {
			continue
		}
		if bytes.Equal(chunk["type"], []byte(`"block-start"`)) {
			for _, retired := range []string{`"tool-result"`, `"tool-addition"`, `"tool-removal"`} {
				if bytes.Equal(chunk["blockType"], []byte(retired)) {
					return errors.New("assistant stream contains retired or developer-only block")
				}
			}
		}
		if bytes.Equal(chunk["type"], []byte(`"block-end"`)) {
			block, err := objectValue(chunk["block"])
			if err != nil {
				continue
			}
			for _, retired := range []string{`"tool-result"`, `"tool-addition"`, `"tool-removal"`} {
				if bytes.Equal(block["type"], []byte(retired)) {
					return errors.New("assistant stream contains retired or developer-only block")
				}
			}
		}
	}
	return nil
}
