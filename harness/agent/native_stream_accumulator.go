package agent

import "math"

// nativeStreamAccumulator is the Go counterpart of DeepSeek's
// AssistantStreamAccumulator. It keeps the original live chunk boundaries for
// observers while packing consecutive same-block deltas in the durable v4
// stream. Non-delta chunks remain exact raw records.
type nativeStreamAccumulator struct {
	records []any
	chunks  int
	last    *nativeStreamRun
}

type nativeStreamRun struct {
	record   map[string]any
	kind     string
	index    int
	id       string
	name     string
	hasName  bool
	lastTime int64
}

func (a *nativeStreamAccumulator) push(chunk ModelStreamChunk, stamp int64) {
	a.chunks++
	kind, _ := chunk["type"].(string)
	indexValue, indexed := chunk["index"].(float64)
	if !indexed || indexValue < 0 || indexValue > math.MaxInt32 || indexValue != math.Trunc(indexValue) {
		a.raw(chunk, stamp)
		return
	}
	index := int(indexValue)
	fragment := ""
	runKind := ""
	id, name := "", ""
	hasName := false
	switch kind {
	case "text-delta", "reasoning-delta":
		fragment, indexed = chunk["text"].(string)
		if !indexed {
			a.raw(chunk, stamp)
			return
		}
		runKind = "text-chunks"
		if kind == "reasoning-delta" {
			runKind = "reasoning-chunks"
		}
	case "tool-call-delta":
		id, indexed = chunk["id"].(string)
		if !indexed || id == "" {
			a.raw(chunk, stamp)
			return
		}
		fragment, indexed = chunk["argumentsDelta"].(string)
		if !indexed {
			a.raw(chunk, stamp)
			return
		}
		name, hasName = chunk["name"].(string)
		if hasName && name == "" {
			a.raw(chunk, stamp)
			return
		}
		runKind = "tool-call-chunks"
	default:
		a.raw(chunk, stamp)
		return
	}
	if run := a.last; run != nil && run.kind == runKind && run.index == index && run.id == id &&
		run.hasName == hasName && run.name == name {
		gap := stamp - run.lastTime
		if gap >= -(1<<53-1) && gap <= 1<<53-1 && run.lastTime+gap == stamp {
			run.record["dt"] = append(run.record["dt"].([]int64), gap)
			if runKind == "tool-call-chunks" {
				run.record["args"] = append(run.record["args"].([]string), fragment)
			} else {
				run.record["texts"] = append(run.record["texts"].([]string), fragment)
			}
			run.lastTime = stamp
			return
		}
	}
	record := map[string]any{"type": runKind, "time0": stamp, "index": index, "dt": []int64{}}
	if runKind == "tool-call-chunks" {
		record["id"], record["args"] = id, []string{fragment}
		if hasName {
			record["name"] = name
		}
	} else {
		record["texts"] = []string{fragment}
	}
	a.records = append(a.records, record)
	a.last = &nativeStreamRun{record: record, kind: runKind, index: index, id: id, name: name, hasName: hasName, lastTime: stamp}
}

func (a *nativeStreamAccumulator) raw(chunk ModelStreamChunk, stamp int64) {
	a.records = append(a.records, map[string]any{"type": "chunk", "time": stamp, "chunk": map[string]any(chunk)})
	a.last = nil
}
