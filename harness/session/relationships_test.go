package sessionformat

import "testing"

func coreToolExchange() []Event {
	return []Event{
		surfaceEvent("turn/start", 0, `{"turn":1}`, "", ""),
		surfaceEvent("step/start", 1, `{"turn":1,"step":1}`, "", ""),
		surfaceEvent("assistant/message", 2, `{"turn":1,"step":1,"stream":[],"message":{"id":"assistant","role":"assistant","source":{"kind":"model","provider":"mock","model":"mock"},"content":[{"type":"tool-call","id":"call-1","name":"readfile","arguments":"{\"path\":\"a\"}"}]}}`, `"append"`, ""),
		surfaceEvent("tool/call", 3, `{"turn":1,"step":1,"callId":"call-1","name":"readfile","arguments":"{\"path\":\"a\"}"}`, "", ""),
		surfaceEvent("tool/result", 4, `{"turn":1,"step":1,"message":{"id":"result","role":"tool","source":{"kind":"tool","callId":"call-1"},"toolCallId":"call-1","content":[{"type":"text","text":"done"}]}}`, `"append"`, ""),
		surfaceEvent("step/end", 5, `{"turn":1,"step":1}`, "", ""),
		surfaceEvent("turn/end", 6, `{"turn":1,"reason":{"kind":"completed"}}`, "", ""),
	}
}

func TestCoreV4RelationshipsRetainToolLifecycleAndOpenTails(t *testing.T) {
	events := coreToolExchange()
	for length := 0; length <= len(events); length++ {
		if err := ValidateCoreRelationships(events[:length]); err != nil {
			t.Fatalf("valid open tail of length %d rejected: %v", length, err)
		}
	}
	if _, err := FoldSurface(events); err != nil {
		t.Fatalf("tool exchange did not form a surface: %v", err)
	}
}

func TestCoreV4RelationshipsRejectMismatchedTurnsStepsAndTools(t *testing.T) {
	good := coreToolExchange()
	for name, events := range map[string][]Event{
		"turn order":        {surfaceEvent("turn/start", 0, `{"turn":2}`, "", "")},
		"step outside turn": {surfaceEvent("step/start", 0, `{"turn":1,"step":1}`, "", "")},
		"header outside turn": {surfaceEvent("request/header", 0,
			`{"header":{"config":{"provider":"mock","model":"mock"}},"reason":"initial"}`, "", "")},
		"unadvertised tool": {good[0], good[1], surfaceEvent("tool/call", 2,
			`{"turn":1,"step":1,"callId":"call-1","name":"readfile","arguments":"{}"}`, "", "")},
		"changed tool name": {good[0], good[1], good[2], surfaceEvent("tool/call", 3,
			`{"turn":1,"step":1,"callId":"call-1","name":"writefile","arguments":"{\"path\":\"a\"}"}`, "", "")},
		"unsettled at step end": {good[0], good[1], good[2], surfaceEvent("step/end", 3,
			`{"turn":1,"step":1}`, "", "")},
		"turn ends with step": {good[0], good[1], surfaceEvent("turn/end", 2,
			`{"turn":1,"reason":{"kind":"completed"}}`, "", "")},
	} {
		if err := ValidateCoreRelationships(events); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestCoreV4RelationshipsPermitOnlyExactNotStartedRepair(t *testing.T) {
	good := coreToolExchange()
	repair := surfaceEvent("tool/result", 3, `{"turn":1,"step":1,"message":{"id":"interrupted-tool-result-call-1-200","role":"tool","source":{"kind":"tool","callId":"call-1"},"toolCallId":"call-1","content":[{"type":"text","text":"The tool call was interrupted before the Harness recorded it as started. Retry it if it is still needed."}],"isError":true},"error":{"name":"ToolNotStartedError","code":"TOOL_NOT_STARTED"}}`, `"append"`, "")
	if err := ValidateCoreRelationships([]Event{good[0], good[1], good[2], repair}); err != nil {
		t.Fatalf("exact TOOL_NOT_STARTED repair rejected: %v", err)
	}
	repair.Data = []byte(`{"turn":1,"step":1,"message":{"id":"invented","role":"tool","source":{"kind":"tool","callId":"call-1"},"toolCallId":"call-1","content":[],"isError":true},"error":{"name":"ToolNotStartedError","code":"TOOL_NOT_STARTED"}}`)
	if err := ValidateCoreRelationships([]Event{good[0], good[1], good[2], repair}); err == nil {
		t.Fatal("invented not-started repair was accepted")
	}
}
