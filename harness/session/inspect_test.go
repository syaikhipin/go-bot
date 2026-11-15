package sessionformat

import (
	"strings"
	"testing"
)

func TestInspectV4JSONLJoinsPhysicalAndSemanticAdmission(t *testing.T) {
	valid := physicalV4Header + "\n" + `{"type":"user/message","seq":0,"time":2,"data":{"id":"u","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"hi"}]},"surfaceOp":"append"}` + "\n"
	inspection, err := InspectV4JSONL(strings.NewReader(valid))
	if err != nil || len(inspection.Events) != 1 || len(inspection.Surface.Messages) != 1 {
		t.Fatalf("valid v4 inspection = %#v, %v", inspection, err)
	}
	for name, raw := range map[string]string{
		"retired source": strings.Replace(valid, `"kind":"user"`, `"kind":"plugin"`, 1),
		"unopened step":  physicalV4Header + "\n" + `{"type":"step/start","seq":0,"time":2,"data":{"turn":1,"step":1}}` + "\n",
		"bad surface":    strings.Replace(valid, `"surfaceOp":"append"`, `"surfaceOp":{"op":"replace","startSeq":0,"endSeq":0}`, 1),
	} {
		if _, err := InspectV4JSONL(strings.NewReader(raw)); err == nil {
			t.Fatalf("%s was admitted", name)
		}
	}
}

func TestInspectV4JSONLRejectsCorruptPendingInbox(t *testing.T) {
	message := `{"id":"same","role":"user","source":{"kind":"user"},"content":[]}`
	log := physicalV4Header + "\n" +
		`{"type":"agent/inbox/spliced","seq":0,"time":2,"data":{"target":"next-turn","start":0,"inserted":[` + message + `]}}` + "\n" +
		`{"type":"agent/inbox/spliced","seq":1,"time":3,"data":{"target":"next-step","start":0,"inserted":[` + message + `]}}` + "\n"
	if _, err := InspectV4JSONL(strings.NewReader(log)); err == nil || !strings.Contains(err.Error(), "already pending") {
		t.Fatalf("duplicate pending input was restored: %v", err)
	}
}
