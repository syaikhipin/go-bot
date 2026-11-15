package agent

import (
	"encoding/json"
	"fmt"
	"sync"
)

// NativeAssistantStreamFrame matches DeepSeek's process-local attempt framing.
// Chunk frames are transient presentation data; only the settled v4 event is
// a recovery source. Revision restarts with a replacement Agent lifecycle.
type NativeAssistantStreamFrame struct {
	Type      string `json:"type"`
	AttemptID string `json:"attemptId"`
	Revision  uint64 `json:"revision"`
	// StartedAfterSeq is set on start frames; the public follower uses it to
	// distinguish a later durable settlement from older replayed events.
	StartedAfterSeq *int64           `json:"startedAfterSeq,omitempty"`
	Turn            uint64           `json:"turn,omitempty"`
	Step            int              `json:"step,omitempty"`
	Index           *int             `json:"index,omitempty"`
	Time            int64            `json:"time,omitempty"`
	Chunk           ModelStreamChunk `json:"chunk,omitempty"`
	Outcome         map[string]any   `json:"outcome,omitempty"`
}

type NativeAssistantStreamBaseline struct {
	Revision      uint64                                `json:"revision"`
	ActiveAttempt *NativeAssistantStreamAttemptBaseline `json:"activeAttempt,omitempty"`
}

type NativeAssistantStreamAttemptBaseline struct {
	AttemptID       string `json:"attemptId"`
	StartedAfterSeq int64  `json:"startedAfterSeq"`
	Turn            uint64 `json:"turn"`
	Step            int    `json:"step"`
	NextIndex       int    `json:"nextIndex"`
	Stream          []any  `json:"stream"`
}

type nativeActiveAssistantAttempt struct {
	id              string
	startedAfterSeq int64
	turn            uint64
	step            int
	stream          *nativeStreamAccumulator
}

// SubscribeAssistantStream observes future frames only. The caller must use
// the durable Session cursor to rebuild state after disconnect. An observer
// that cannot keep up is closed; it must reconnect and use that cursor.
func (a *NativeLoop) SubscribeAssistantStream() (<-chan NativeAssistantStreamFrame, func()) {
	_, frames, cancel := a.SubscribeAssistantStreamSnapshot()
	return frames, cancel
}

// SubscribeAssistantStreamSnapshot atomically registers a follower and gives
// it the latest revision and compact active prefix. A reconnect can discard
// its previous provisional rows and continue from this new baseline.
func (a *NativeLoop) SubscribeAssistantStreamSnapshot() (NativeAssistantStreamBaseline, <-chan NativeAssistantStreamFrame, func()) {
	channel := make(chan NativeAssistantStreamFrame, 256)
	a.streamMu.Lock()
	if a.streamSubscribers == nil {
		a.streamSubscribers = make(map[chan NativeAssistantStreamFrame]struct{})
	}
	baseline := a.assistantStreamBaselineLocked()
	a.streamSubscribers[channel] = struct{}{}
	a.streamMu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			a.streamMu.Lock()
			if _, exists := a.streamSubscribers[channel]; exists {
				delete(a.streamSubscribers, channel)
				close(channel)
			}
			a.streamMu.Unlock()
		})
	}
	return baseline, channel, cancel
}

func (a *NativeLoop) assistantStreamBaselineLocked() NativeAssistantStreamBaseline {
	baseline := NativeAssistantStreamBaseline{Revision: a.streamRevision}
	active := a.streamActive
	if active == nil {
		return baseline
	}
	encoded, _ := json.Marshal(active.stream.records)
	var records []any
	_ = json.Unmarshal(encoded, &records)
	if records == nil {
		records = []any{}
	}
	baseline.ActiveAttempt = &NativeAssistantStreamAttemptBaseline{
		AttemptID: active.id, StartedAfterSeq: active.startedAfterSeq,
		Turn: active.turn, Step: active.step, NextIndex: active.stream.chunks,
		Stream: records,
	}
	return baseline
}

func (a *NativeLoop) publishStreamFrameLocked(frame NativeAssistantStreamFrame) {
	a.streamRevision++
	frame.Revision = a.streamRevision
	for channel := range a.streamSubscribers {
		select {
		case channel <- frame:
		default:
			delete(a.streamSubscribers, channel)
			close(channel)
		}
	}
}

func (a *NativeLoop) startStreamAttempt(turn uint64, step int, stream *nativeStreamAccumulator) string {
	startedAfterSeq := a.session.DurableCursor()
	a.streamMu.Lock()
	defer a.streamMu.Unlock()
	a.streamAttemptCounter++
	id := fmt.Sprintf("%s:%d", a.session.Header().ID, a.streamAttemptCounter)
	a.streamActive = &nativeActiveAssistantAttempt{id: id, startedAfterSeq: startedAfterSeq, turn: turn, step: step, stream: stream}
	a.publishStreamFrameLocked(NativeAssistantStreamFrame{Type: "start", AttemptID: id,
		StartedAfterSeq: &startedAfterSeq, Turn: turn, Step: step})
	return id
}

func (a *NativeLoop) endStreamAttempt(attemptID string, index int, eventType string, seq uint64, committed bool) {
	if attemptID == "" {
		return
	}
	outcome := map[string]any{"kind": "abandoned"}
	if committed {
		outcome = map[string]any{"kind": "committed", "eventType": eventType, "seq": seq}
	}
	a.streamMu.Lock()
	if a.streamActive != nil && a.streamActive.id == attemptID {
		a.streamActive = nil
	}
	a.publishStreamFrameLocked(NativeAssistantStreamFrame{Type: "end", AttemptID: attemptID, Index: &index, Outcome: outcome})
	a.streamMu.Unlock()
}
