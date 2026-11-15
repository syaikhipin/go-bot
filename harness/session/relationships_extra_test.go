package sessionformat

import (
	"encoding/json"
	"testing"
)

func resequence(events ...Event) []Event {
	for index := range events {
		events[index].Seq = uint64(index)
		events[index].Time = int64(index + 1)
	}
	return events
}

func TestV4CompactionRelationshipsAndOpenTails(t *testing.T) {
	rows := resequence(
		surfaceEvent("turn/start", 0, `{"turn":1}`, "", ""),
		surfaceEvent("user/message", 0, `{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"long history"}]}`, `"append"`, ""),
		surfaceEvent("compaction/start", 0, `{"compactionId":"c","turn":1,"sourceCommandId":"cmd"}`, "", ""),
		surfaceEvent("compaction/summary", 0, `{"compactionId":"c","sourceCommandId":"cmd","summary":[{"type":"text","text":"summary"}],"shadowedRange":{"start":1,"end":1},"shadowedSeqs":[1],"shadowedTokenCount":1,"provider":"mock","model":"mock"}`, "", ""),
		surfaceEvent("user/message", 0, `{"id":"checkpoint","role":"user","source":{"kind":"compact-checkpoint","compactionId":"c","sourceCommandId":"cmd"},"content":[{"type":"text","text":"summary"}]}`, `{"op":"replace","startSeq":1,"endSeq":1}`, `[1,2,3]`),
		surfaceEvent("compaction/end", 0, `{"compactionId":"c","sourceCommandId":"cmd","turn":1}`, "", ""),
		surfaceEvent("turn/end", 0, `{"turn":1,"reason":{"kind":"completed"}}`, "", ""),
	)
	for length := 0; length <= len(rows); length++ {
		if err := ValidateV4Relationships(rows[:length]); err != nil {
			t.Fatalf("valid compaction tail length %d rejected: %v", length, err)
		}
	}
	bad := append([]Event(nil), rows...)
	bad[3].Data = json.RawMessage(`{"compactionId":"wrong","sourceCommandId":"cmd","shadowedRange":{"start":1,"end":1},"shadowedSeqs":[1]}`)
	if err := ValidateV4Relationships(bad); err == nil {
		t.Fatal("compaction summary with wrong owner accepted")
	}
	bad = append([]Event(nil), rows[:3]...)
	bad = append(bad, surfaceEvent("turn/end", 3, `{"turn":1,"reason":{"kind":"completed"}}`, "", ""))
	if err := ValidateV4Relationships(bad); err == nil {
		t.Fatal("turn crossed a live compaction")
	}
	bad = resequence(rows[0], rows[1], rows[2], rows[5])
	if err := ValidateV4Relationships(bad); err == nil {
		t.Fatal("successful compaction without summary accepted")
	}
	orphan := resequence(
		surfaceEvent("compaction/start", 0, `{"compactionId":"old","turn":null}`, "", ""),
		surfaceEvent("turn/start", 0, `{"turn":1}`, "", ""),
		surfaceEvent("turn/end", 0, `{"turn":1,"reason":{"kind":"completed"}}`, "", ""),
		surfaceEvent("session/end-seed", 0, `{"inherited":true}`, "", ""),
	)
	if err := ValidateV4Relationships(orphan); err != nil {
		t.Fatalf("inherited orphan compaction rejected: %v", err)
	}
}

func TestV4RetryAndPTCRelationships(t *testing.T) {
	rows := resequence(
		surfaceEvent("turn/start", 0, `{"turn":1}`, "", ""),
		surfaceEvent("step/start", 0, `{"turn":1,"step":1}`, "", ""),
		surfaceEvent("request/header", 0, `{"header":{"config":{"provider":"mock","model":"mock"}},"reason":"initial"}`, "", ""),
		surfaceEvent("llm/retry", 0, `{"turn":1,"step":1,"retryId":"retry-1","retry":1,"provider":"mock","policyKey":"policy"}`, "", ""),
		surfaceEvent("llm/retry-started", 0, `{"turn":1,"step":1,"retryId":"retry-1","retry":1}`, "", ""),
		surfaceEvent("tool/ptc-dispatch-start", 0, `{"rootCallId":"root","parentCallId":"root","subCallId":"child","name":"readfile","arguments":{"a":1,"b":2}}`, "", ""),
		surfaceEvent("tool/ptc-dispatch", 0, `{"rootCallId":"root","parentCallId":"root","subCallId":"child","name":"readfile","arguments":{"b":2,"a":1}}`, "", ""),
		surfaceEvent("step/end", 0, `{"turn":1,"step":1}`, "", ""),
		surfaceEvent("turn/end", 0, `{"turn":1,"reason":{"kind":"completed"}}`, "", ""),
	)
	if err := ValidateV4Relationships(rows); err != nil {
		t.Fatalf("valid retry/PTC sequence rejected: %v", err)
	}
	bad := append([]Event(nil), rows...)
	bad[4].Data = json.RawMessage(`{"turn":1,"step":1,"retryId":"retry-1","retry":2}`)
	if err := ValidateV4Relationships(bad); err == nil {
		t.Fatal("unscheduled retry start accepted")
	}
	bad = append([]Event(nil), rows...)
	bad[6].Data = json.RawMessage(`{"rootCallId":"root","parentCallId":"root","subCallId":"child","name":"writefile","arguments":{"a":1,"b":2}}`)
	if err := ValidateV4Relationships(bad); err == nil {
		t.Fatal("PTC settlement changed its tool name")
	}
}

func TestV4TitleCommandAndDeveloperRelationships(t *testing.T) {
	user := surfaceEvent("user/message", 0, `{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"make a file"}]}`, `"append"`, "")
	command := surfaceEvent("command/run", 0, `{"commandId":"cmd"}`, "", "")
	done := surfaceEvent("command/done", 0, `{"commandId":"cmd","kind":"success","sourceEventSeq":0}`, "", "")
	title := surfaceEvent("session/title", 0, `{"title":"Make a file","messageSeqs":[0],"source":{"kind":"model"}}`, "", "")
	rows := resequence(user, command, done, title)
	if err := ValidateV4Relationships(rows); err != nil {
		t.Fatalf("valid title/command sequence rejected: %v", err)
	}
	bad := append([]Event(nil), rows...)
	bad[2].Data = json.RawMessage(`{"commandId":"cmd","kind":"success","sourceEventSeq":1}`)
	if err := ValidateV4Relationships(bad); err == nil {
		t.Fatal("command/done cited command/run as its output source")
	}
	bad = append([]Event(nil), rows...)
	bad[3].Data = json.RawMessage(`{"title":"bad","messageSeqs":[1],"source":{"kind":"model"}}`)
	if err := ValidateV4Relationships(bad); err == nil {
		t.Fatal("title cited a non-human message")
	}
	developer := resequence(
		surfaceEvent("turn/start", 0, `{"turn":1}`, "", ""),
		surfaceEvent("step/start", 0, `{"turn":1,"step":1}`, "", ""),
		surfaceEvent("request/header", 0, `{"header":{"config":{"provider":"mock","model":"mock"},"tools":[{"name":"readfile","description":"Read a file","parameters":{"type":"object"}}]},"reason":"initial"}`, "", ""),
		surfaceEvent("developer/message", 0, `{"turn":1,"step":1,"headerSeq":2,"message":{"id":"d1","role":"developer","source":{"kind":"agent"},"content":[{"type":"tool-addition","toolName":"readfile"}]}}`, `"append"`, ""),
	)
	if err := ValidateV4Relationships(developer); err != nil {
		t.Fatalf("valid developer tool addition rejected: %v", err)
	}
	developer[3].Data = json.RawMessage(`{"turn":1,"step":1,"headerSeq":2,"message":{"id":"d1","role":"developer","source":{"kind":"agent"},"content":[{"type":"tool-addition","toolName":"writefile"}]}}`)
	if err := ValidateV4Relationships(developer); err == nil {
		t.Fatal("developer addition cited absent header tool")
	}
}
