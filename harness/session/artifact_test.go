package sessionformat

import (
	"strings"
	"testing"
)

const physicalV4Header = `{"type":"session","version":4,"id":"session-1","createdAt":1,"cwd":"/workspace","isSeeded":false,"delegationDepth":0}`

func TestDecodePhysicalV4AndRestoreModelSurface(t *testing.T) {
	rows := []string{
		physicalV4Header,
		`{"type":"turn/start","seq":0,"time":2,"data":{"turn":1}}`,
		`{"type":"step/start","seq":1,"time":3,"data":{"turn":1,"step":1}}`,
		`{"type":"system/message","seq":2,"time":4,"data":{"turn":1,"step":1,"message":{"id":"system-1","role":"system","source":{"kind":"system-prompt"},"content":[{"type":"text","text":"Help the user"}]}},"surfaceOp":"append"}`,
		`{"type":"user/message","seq":3,"time":5,"data":{"id":"user-1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"Hello"}]},"surfaceOp":"append"}`,
		`{"type":"assistant/message","seq":4,"time":6,"data":{"turn":1,"step":1,"message":{"id":"assistant-1","role":"assistant","source":{"kind":"model","provider":"mock","model":"mock"},"content":[{"type":"text","text":"Hi"}]},"stream":[]},"surfaceOp":"append"}`,
		`{"type":"step/end","seq":5,"time":7,"data":{"turn":1,"step":1}}`,
		`{"type":"turn/end","seq":6,"time":8,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
	}
	artifact, err := DecodePhysicalJSONL(strings.NewReader(strings.Join(rows, "\n") + "\n"))
	if err != nil || artifact.Header.ID != "session-1" || len(artifact.Events) != 7 || artifact.InheritedEventCount != 0 {
		t.Fatalf("decoded physical v4 artifact = %#v, %v", artifact, err)
	}
	surface, err := FoldSurface(artifact.Events)
	if err != nil || len(surface.Nodes) != 3 || len(surface.Messages) != 3 {
		t.Fatalf("model-visible v4 surface = %#v, %v", surface, err)
	}
}

func TestDecodePhysicalV4TailAndRequiredEventAdmission(t *testing.T) {
	for name, input := range map[string]string{
		"torn tail":       physicalV4Header + "\n" + `{"type":"turn/start","seq":0`,
		"complete event":  physicalV4Header + "\n" + `{"type":"turn/start","seq":0,"time":2,"data":{"turn":1}}` + "\n",
		"buffer boundary": physicalV4Header + "\n" + `{"type":"workspace/changes","seq":0,"time":2,"data":{"text":"` + strings.Repeat("x", 70<<10) + `"}}` + "\n",
	} {
		artifact, err := DecodePhysicalJSONL(strings.NewReader(input))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "torn tail" && len(artifact.Events) != 0 || name != "torn tail" && len(artifact.Events) != 1 {
			t.Fatalf("%s: decoded %d events", name, len(artifact.Events))
		}
	}
	for name, row := range map[string]string{
		"invalid complete row": `{"type":"turn/start","seq":0,"time":2,"data":`,
		"unknown required":     `{"type":"future/essential","seq":0,"time":2,"data":{}}`,
		"sequence gap":         `{"type":"turn/start","seq":1,"time":2,"data":{"turn":1}}`,
		"custom fence row":     `{"kind":"fence","epochId":"e"}`,
	} {
		if _, err := DecodePhysicalJSONL(strings.NewReader(physicalV4Header + "\n" + row + "\n")); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	ignored := `{"type":"future/informational","seq":0,"time":2,"data":{"detail":"keep"},"ignorable":true}`
	artifact, err := DecodePhysicalJSONL(strings.NewReader(physicalV4Header + "\n" + ignored + "\n"))
	if err != nil || len(artifact.Events) != 1 || !artifact.Events[0].Ignorable {
		t.Fatalf("unknown ignorable event was lost: %#v, %v", artifact, err)
	}
}

func TestDecodePhysicalV4SeedCut(t *testing.T) {
	header := `{"type":"session","version":4,"id":"child-1","createdAt":1,"cwd":"/workspace","parentSession":"root-1","isSeeded":true,"origin":"subagent","delegationDepth":1}`
	rows := []string{
		header,
		`{"type":"turn/start","seq":0,"time":2,"data":{"turn":1}}`,
		`{"type":"turn/end","seq":1,"time":3,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
		`{"type":"session/end-seed","seq":2,"time":4,"data":{"inherited":true}}`,
	}
	artifact, err := DecodePhysicalJSONL(strings.NewReader(strings.Join(rows, "\n") + "\n"))
	if err != nil || artifact.InheritedEventCount != 2 {
		t.Fatalf("seeded v4 cut = %#v, %v", artifact, err)
	}
	if _, err := DecodePhysicalJSONL(strings.NewReader(strings.Join(rows[:3], "\n") + "\n")); err == nil {
		t.Fatal("seeded header without inherited marker was accepted")
	}
	unseeded := strings.Replace(physicalV4Header, `"isSeeded":false`, `"isSeeded":true`, 1)
	if _, err := DecodePhysicalJSONL(strings.NewReader(unseeded + "\n" + rows[3] + "\n")); err == nil {
		t.Fatal("seeded header with a sequence gap was accepted")
	}
	replayed, err := DecodePhysicalJSONL(strings.NewReader(physicalV4Header + "\n" + `{"type":"session/end-seed","seq":0,"time":2,"data":{"inherited":true}}` + "\n"))
	if err != nil || replayed.InheritedEventCount != 0 || replayed.Header.IsSeeded {
		t.Fatal("a replayed prior marker invented new fork lineage", err)
	}
}
