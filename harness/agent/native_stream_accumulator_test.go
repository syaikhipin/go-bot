package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNativeStreamAccumulatorMatchesDeepSeekCompactRunShape(t *testing.T) {
	var accumulator nativeStreamAccumulator
	for _, item := range []struct {
		chunk ModelStreamChunk
		time  int64
	}{
		{ModelStreamChunk{"type": "block-start", "index": float64(0), "blockType": "text"}, 100},
		{ModelStreamChunk{"type": "text-delta", "index": float64(0), "text": "a"}, 101},
		{ModelStreamChunk{"type": "text-delta", "index": float64(0), "text": "b"}, 105},
		{ModelStreamChunk{"type": "block-end", "index": float64(0), "block": ModelStreamChunk{"type": "text", "text": "ab"}}, 106},
		{ModelStreamChunk{"type": "tool-call-delta", "index": float64(1), "id": "call-1", "name": "readfile", "argumentsDelta": "{"}, 107},
		{ModelStreamChunk{"type": "tool-call-delta", "index": float64(1), "id": "call-1", "name": "readfile", "argumentsDelta": "}"}, 110},
	} {
		accumulator.push(item.chunk, item.time)
	}
	if accumulator.chunks != 6 || len(accumulator.records) != 4 {
		t.Fatalf("chunk/run counts = %d/%d", accumulator.chunks, len(accumulator.records))
	}
	encoded, err := json.Marshal(accumulator.records)
	if err != nil {
		t.Fatal(err)
	}
	var actual []map[string]any
	if err := json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	textRun := actual[1]
	toolRun := actual[3]
	if !reflect.DeepEqual(textRun, map[string]any{"type": "text-chunks", "time0": float64(101), "index": float64(0),
		"dt": []any{float64(4)}, "texts": []any{"a", "b"}}) ||
		!reflect.DeepEqual(toolRun, map[string]any{"type": "tool-call-chunks", "time0": float64(107), "index": float64(1),
			"dt": []any{float64(3)}, "id": "call-1", "name": "readfile", "args": []any{"{", "}"}}) {
		t.Fatalf("compacted stream differs from original record shapes: %s", encoded)
	}
	content, blocks, _ := nativeInterruptedPrefix(accumulator.records)
	if content != "ab" || len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "ab" {
		t.Fatalf("interrupted compacted prefix = %q, %+v", content, blocks)
	}
}
