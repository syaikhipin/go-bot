package sessionformat

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func surfaceEvent(eventType string, seq uint64, data, op, sources string) Event {
	return Event{Type: eventType, Seq: seq, Time: int64(seq + 1), Data: json.RawMessage(data), SurfaceOp: json.RawMessage(op), SourceEventSeqs: json.RawMessage(sources)}
}

func TestFoldV4SurfaceUsesCurrentNodesNotAllHistoricalMessages(t *testing.T) {
	events := []Event{
		surfaceEvent("user/message", 0, `{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"old"}]}`, `"append"`, ""),
		surfaceEvent("assistant/message", 1, `{"turn":1,"step":1,"message":{"id":"a1","role":"assistant","source":{"kind":"model","provider":"mock","model":"mock"},"content":[{"type":"text","text":"old answer"}]},"stream":[]}`, `"append"`, ""),
		surfaceEvent("user/message", 2, `{"id":"summary","role":"user","source":{"kind":"compact-checkpoint","compactionId":"c1"},"content":[{"type":"text","text":"summary"}]}`, `{"op":"replace","startSeq":0,"endSeq":1}`, `[0,1]`),
	}
	surface, err := FoldSurface(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface.Nodes) != 1 || surface.Nodes[0] != 2 || len(surface.Messages) != 1 || !strings.Contains(string(surface.Messages[0]), `"summary"`) {
		t.Fatalf("incorrect restored model surface: %#v", surface)
	}
	if len(surface.Replacements) != 1 || len(surface.Replacements[0].ShadowedSeqs) != 2 {
		t.Fatalf("replacement history was lost: %#v", surface.Replacements)
	}
}

func TestFoldV4SurfaceRequiresCompleteReplacementAttribution(t *testing.T) {
	first := surfaceEvent("user/message", 0, `{"id":"u1","role":"user","source":{"kind":"user"},"content":[]}`, `"append"`, "")
	second := surfaceEvent("user/message", 1, `{"id":"u2","role":"user","source":{"kind":"user"},"content":[]}`, `"append"`, "")
	bad := surfaceEvent("user/message", 2, `{"id":"u3","role":"user","source":{"kind":"user"},"content":[]}`, `{"op":"replace","startSeq":0,"endSeq":1}`, `[0]`)
	if _, err := FoldSurface([]Event{first, second, bad}); err == nil || !strings.Contains(err.Error(), "shadowed source 1") {
		t.Fatalf("incomplete replacement accepted: %v", err)
	}
	bad.SourceEventSeqs = json.RawMessage(`[[0,1]]`)
	if surface, err := FoldSurface([]Event{first, second, bad}); err != nil || len(surface.Nodes) != 1 || surface.Nodes[0] != 2 {
		t.Fatalf("packed source range not replayed: %#v, %v", surface, err)
	}
	bad.SurfaceOp = json.RawMessage(`{"op":"replace","startSeq":0,"endSeq":3}`)
	if _, err := FoldSurface([]Event{first, second, bad}); err == nil {
		t.Fatal("replacement outside the current surface accepted")
	}
}

func TestFoldV4SurfaceProtectsSystemHeadAndOmitsEmptyMessage(t *testing.T) {
	system := surfaceEvent("system/message", 0, `{"turn":1,"step":1,"message":{"id":"s1","role":"system","source":{"kind":"system"},"content":[{"type":"text","text":"prompt"}]}}`, `"append"`, "")
	user := surfaceEvent("user/message", 1, `{"id":"u1","role":"user","source":{"kind":"user"},"content":[]}`, `"append"`, "")
	bad := surfaceEvent("user/message", 2, `{"id":"u2","role":"user","source":{"kind":"user"},"content":[]}`, `{"op":"replace","startSeq":0,"endSeq":1}`, `[0,1]`)
	if _, err := FoldSurface([]Event{system, user, bad}); err == nil || !strings.Contains(err.Error(), "system prompt head") {
		t.Fatalf("system head was not protected: %v", err)
	}
	update := surfaceEvent("system/message", 2, `{"turn":1,"step":1,"message":{"id":"s2","role":"system","source":{"kind":"system"},"content":[]}}`, `{"op":"replace","startSeq":0,"endSeq":0}`, `[0]`)
	surface, err := FoldSurface([]Event{system, user, update})
	if err != nil || len(surface.Nodes) != 2 || len(surface.Messages) != 1 || !strings.Contains(string(surface.Messages[0]), `"u1"`) {
		t.Fatalf("empty system prompt was not omitted: %#v, %v", surface, err)
	}
}

func TestFoldV4SurfaceRestrictsToolResultRewrite(t *testing.T) {
	result := surfaceEvent("tool/result", 0, `{"turn":1,"step":1,"message":{"id":"r1","role":"tool","source":{"kind":"tool","callId":"c1"},"toolCallId":"c1","content":[{"type":"text","text":"long"}]}}`, `"append"`, "")
	rewrite := surfaceEvent("tool/result", 1, `{"turn":1,"step":1,"message":{"id":"r1","role":"tool","source":{"kind":"tool","callId":"c1"},"toolCallId":"c1","content":[{"type":"text","text":"short"}]}}`, `{"op":"replace","startSeq":0,"endSeq":0}`, `[0]`)
	if surface, err := FoldSurface([]Event{result, rewrite}); err != nil || len(surface.Messages) != 1 || !strings.Contains(string(surface.Messages[0]), `"short"`) {
		t.Fatalf("valid tool-result rewrite failed: %#v, %v", surface, err)
	}
	rewrite.Data = json.RawMessage(strings.Replace(string(rewrite.Data), `"c1"`, `"c2"`, 1))
	if _, err := FoldSurface([]Event{result, rewrite}); err == nil {
		t.Fatal("tool-result identity change was accepted")
	}
}

func TestFoldV4SurfaceReplaysImageOffload(t *testing.T) {
	ignored := surfaceEvent("plugin:example/info", 0, `null`, `{"future":true}`, "")
	ignored.Ignorable = true
	if surface, err := FoldSurface([]Event{ignored}); err != nil || len(surface.Nodes) != 0 {
		t.Fatalf("opaque ignorable event changed the surface: %#v, %v", surface, err)
	}
	user := surfaceEvent("user/message", 0, `{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"image","attachment":{"attachmentId":"a"}},{"type":"image","attachment":{"attachmentId":"a"}}]}`, `"append"`, "")
	offload := surfaceEvent("image/offload", 1, `{"targets":[{"seq":0,"imageIndexes":[1]}]}`, "", "")
	surface, err := FoldSurface([]Event{user, offload})
	if err != nil || len(surface.Messages) != 1 || strings.Count(string(surface.Messages[0]), `"offloaded":true`) != 1 {
		t.Fatalf("image offload projection = %#v, %v", surface, err)
	}
	if _, err := FoldSurface([]Event{user, offload, surfaceEvent("image/offload", 2, string(offload.Data), "", "")}); err == nil {
		t.Fatal("the same image was offloaded twice")
	}
	for _, bad := range []string{
		`{"targets":[{"seq":0,"imageIndexes":[1,1]}]}`,
		`{"targets":[{"seq":0,"imageIndexes":[2]}]}`,
		`{"targets":[{"seq":1,"imageIndexes":[0]}]}`,
	} {
		if _, err := FoldSurface([]Event{user, surfaceEvent("image/offload", 1, bad, "", "")}); err == nil {
			t.Fatalf("invalid image/offload accepted: %s", bad)
		}
	}
}

func TestSurfaceStateIncrementalAppendIsAtomicAndDetached(t *testing.T) {
	first := surfaceEvent("user/message", 0, `{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"first"}]}`, `"append"`, "")
	second := surfaceEvent("user/message", 1, `{"id":"u2","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"second"}]}`, `"append"`, "")
	state, err := NewSurfaceState(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Append(first); err != nil {
		t.Fatal(err)
	}
	first.Data[0] = 'X' // caller mutation cannot change the accepted event
	before, err := state.Snapshot()
	if err != nil || !strings.Contains(string(before.Messages[0]), `"first"`) {
		t.Fatalf("accepted event was aliased: %#v, %v", before, err)
	}
	bad := surfaceEvent("user/message", 1, `{"id":"bad","role":"user","source":{"kind":"user"},"content":[]}`, `{"op":"replace","startSeq":0,"endSeq":0}`, `[1]`)
	if err := state.Append(bad); err == nil {
		t.Fatal("invalid replacement was accepted")
	}
	after, err := state.Snapshot()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected event mutated state: before=%#v after=%#v err=%v", before, after, err)
	}
	if err := state.Append(second); err != nil {
		t.Fatalf("rejected event consumed sequence: %v", err)
	}
	incremental, err := state.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	first.Data[0] = '{'
	refolded, err := FoldSurface([]Event{surfaceEvent("user/message", 0, `{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"first"}]}`, `"append"`, ""), second})
	if err != nil || !reflect.DeepEqual(incremental, refolded) {
		t.Fatalf("incremental surface differs from replay: %#v / %#v, %v", incremental, refolded, err)
	}
	incremental.Messages[0][0] = 'X'
	stable, err := state.Snapshot()
	if err != nil || !strings.HasPrefix(string(stable.Messages[0]), "{") {
		t.Fatalf("snapshot leaked mutable message: %#v, %v", stable, err)
	}
}
