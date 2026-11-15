package sessionformat

import (
	"encoding/json"
	"testing"
)

func TestV4RowAdmissionAcceptsNativeMessages(t *testing.T) {
	rows := []Event{
		surfaceEvent("system/message", 0, `{"turn":1,"step":1,"message":{"id":"system","role":"system","source":{"kind":"system-prompt"},"content":[{"type":"text","text":"Help"}]}}`, `"append"`, ""),
		surfaceEvent("user/message", 1, `{"id":"user","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"Hi"}]}`, `"append"`, ""),
		surfaceEvent("tool/result", 2, `{"turn":1,"step":1,"message":{"id":"result","role":"tool","source":{"kind":"tool","callId":"call"},"toolCallId":"call","content":[{"type":"text","text":"done"}],"isError":false}}`, `"append"`, ""),
		surfaceEvent("request/header", 3, `{"header":{"config":{"provider":"mock","model":"mock"},"tools":[{"name":"readfile","description":"Read","parameters":{"type":"object"},"deferLoading":true}]},"reason":"initial"}`, "", ""),
	}
	if err := ValidateV4Rows(rows); err != nil {
		t.Fatalf("native v4 rows rejected: %v", err)
	}
}

func TestV4RowAdmissionRejectsRetiredFieldsAndSources(t *testing.T) {
	for name, row := range map[string]Event{
		"retired header system":   surfaceEvent("request/header", 0, `{"header":{"config":{"provider":"mock","model":"mock"},"system":"old"},"reason":"initial"}`, "", ""),
		"empty tools":             surfaceEvent("request/header", 0, `{"header":{"config":{"provider":"mock","model":"mock"},"tools":[]},"reason":"initial"}`, "", ""),
		"spaced empty tools":      surfaceEvent("request/header", 0, `{"header":{"config":{"provider":"mock","model":"mock"},"tools":[ ]},"reason":"initial"}`, "", ""),
		"empty adapter defaults":  surfaceEvent("request/header", 0, `{"header":{"config":{"provider":"mock","model":"mock"},"adapterDefaults":{ }},"reason":"initial"}`, "", ""),
		"invalid defer loading":   surfaceEvent("request/header", 0, `{"header":{"config":{"provider":"mock","model":"mock"},"tools":[{"name":"readfile","deferLoading":false}]},"reason":"initial"}`, "", ""),
		"plugin source":           surfaceEvent("user/message", 0, `{"id":"u","role":"user","source":{"kind":"plugin","plugin":"legacy"},"content":[]}`, `"append"`, ""),
		"developer role in user":  surfaceEvent("user/message", 0, `{"id":"u","role":"developer","source":{"kind":"user"},"content":[]}`, `"append"`, ""),
		"developer block in user": surfaceEvent("user/message", 0, `{"id":"u","role":"user","source":{"kind":"user"},"content":[{"type":"tool-addition","toolName":"readfile"}]}`, `"append"`, ""),
		"retired result wrapper":  surfaceEvent("tool/result", 0, `{"turn":1,"step":1,"message":{"id":"r","role":"tool","source":{"kind":"tool","callId":"c"},"toolCallId":"c","content":[{"type":"tool-result","toolCallId":"c","content":[]}]}}`, `"append"`, ""),
		"tool source mismatch":    surfaceEvent("tool/result", 0, `{"turn":1,"step":1,"message":{"id":"r","role":"tool","source":{"kind":"tool","callId":"other"},"toolCallId":"c","content":[]}}`, `"append"`, ""),
		"unflagged tool error":    surfaceEvent("tool/result", 0, `{"turn":1,"step":1,"message":{"id":"r","role":"tool","source":{"kind":"tool","callId":"c"},"toolCallId":"c","content":[]},"error":{"code":"FAIL"}}`, `"append"`, ""),
		"old stream result":       surfaceEvent("assistant/attempt", 0, `{"turn":1,"step":1,"stream":[{"type":"chunk","chunk":{"type":"block-start","blockType":"tool-result"}}]}`, "", ""),
	} {
		if err := ValidateV4Rows([]Event{row}); err == nil {
			t.Fatalf("%s was admitted", name)
		}
	}
}

func TestV4SystemAndDeveloperAdmission(t *testing.T) {
	system := surfaceEvent("system/message", 0, `{"turn":1,"step":1,"message":{"id":"s","role":"system","source":{"kind":"system-prompt"},"content":[{"type":"image","attachment":{"attachmentId":"img","mediaType":"image/png","bytes":3,"width":1,"height":1}}]}}`, `"append"`, "")
	if err := ValidateV4Rows([]Event{system}); err != nil {
		t.Fatalf("valid system image rejected: %v", err)
	}
	system.Data = json.RawMessage(`{"turn":1,"step":1,"message":{"id":"s","role":"system","source":{"kind":"system-prompt"},"content":[{"type":"image","attachment":{"attachmentId":"img","mediaType":"image/png","bytes":3,"width":0,"height":1}}]}}`)
	if err := ValidateV4Rows([]Event{system}); err == nil {
		t.Fatal("zero-width system image was admitted")
	}
	developer := surfaceEvent("developer/message", 0, `{"turn":1,"step":1,"headerSeq":0,"message":{"id":"d","role":"developer","source":{"kind":"agent"},"content":[{"type":"tool-addition","toolName":"readfile"}]}}`, `"append"`, "")
	if err := ValidateV4Rows([]Event{developer}); err != nil {
		t.Fatalf("valid developer tool addition rejected: %v", err)
	}
	developer.Data = json.RawMessage(`{"turn":1,"step":1,"message":{"id":"d","role":"developer","source":{"kind":"agent"},"content":[{"type":"tool-addition","toolName":"readfile"}]}}`)
	if err := ValidateV4Rows([]Event{developer}); err == nil {
		t.Fatal("developer addition without headerSeq was admitted")
	}
}

func TestV4ForkResultAdmission(t *testing.T) {
	result := surfaceEvent("tool/result", 5, `{"turn":1,"step":1,"message":{"id":"forked-tool-result-call-5","role":"tool","source":{"kind":"tool","callId":"call"},"toolCallId":"call","content":[{"type":"text","text":"not started"}],"isError":true},"error":{"name":"ToolNotStartedError","code":"TOOL_NOT_STARTED"}}`, `"append"`, "")
	if err := ValidateV4Rows([]Event{result}); err != nil {
		t.Fatalf("valid fork repair rejected: %v", err)
	}
	result.Data = json.RawMessage(`{"turn":1,"step":1,"message":{"id":"forked-tool-result-call-4","role":"tool","source":{"kind":"tool","callId":"call"},"toolCallId":"call","content":[{"type":"text","text":"not started"}],"isError":true},"error":{"name":"ToolNotStartedError","code":"TOOL_NOT_STARTED"}}`)
	if err := ValidateV4Rows([]Event{result}); err == nil {
		t.Fatal("fork repair with wrong sequence was admitted")
	}
}
