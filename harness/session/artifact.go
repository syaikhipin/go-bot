package sessionformat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// PhysicalArtifact is a decoded, uncompressed v4 JSONL artifact. It is not yet
// a live Session: lifecycle, tool, compaction, and product-owned projections
// must also be admitted before the Agent can resume from it.
type PhysicalArtifact struct {
	Header              Header
	Events              []Event
	InheritedEventCount uint64
}

// The list follows the current DeepSeek Session persistence catalog. Unknown
// events are safe to omit only when the writer explicitly marked them ignorable.
var knownV4EventTypes = map[string]bool{
	"agent-preset/selected": true, "agent/inbox/spliced": true,
	"approval/asked": true, "approval/decided": true, "approval/policy": true,
	"assistant/attempt": true, "assistant/message": true,
	"command/done": true, "command/run": true,
	"compaction/end": true, "compaction/prune": true, "compaction/start": true, "compaction/summary": true,
	"deliverables/presented": true, "developer/message": true,
	"feedback/message-delete": true, "feedback/message-put": true, "feedback/record": true,
	"goal/change": true, "hook/invoked": true, "hook/result": true,
	"image/offload": true, "llm/retry": true, "llm/retry-started": true,
	"model/selection": true, "permission/preset": true, "plan/mode": true,
	"request/context": true, "request/header": true, "sandbox/mode": true,
	"schedule/change": true, "session-log-deepseek/delivery-accepted": true,
	"session/end-seed": true, "session/title": true, "session/title-llm-request": true,
	"step/end": true, "step/start": true,
	"subagent/catalog": true, "subagent/descriptor": true, "subagent/model-selection-policy": true,
	"system/message": true, "team/member": true, "team/message/delivered": true,
	"team/message/queued": true, "team/task": true, "todo/write": true,
	"tool-workflow/agent-end": true, "tool-workflow/agent-start": true,
	"tool-workflow/run-end": true, "tool-workflow/run-start": true,
	"tool/call": true, "tool/ptc-dispatch": true, "tool/ptc-dispatch-start": true,
	"tool/result": true, "turn/end": true, "turn/start": true,
	"user/message": true, "web/deepseek-search-llm-request": true,
	"working-directory/change": true, "workspace/changes": true,
}

// DecodePhysicalJSONL reads only complete newline-terminated rows. Like the
// released JSONL scanner, it ignores a final unterminated fragment, but never
// swallows an invalid complete event or an unknown required event.
func DecodePhysicalJSONL(input io.Reader) (PhysicalArtifact, error) {
	var artifact PhysicalArtifact
	reader := bufio.NewReaderSize(input, 64<<10)
	var line []byte
	row := 0
	var inheritedMarker *uint64
	for {
		fragment, err := reader.ReadSlice('\n')
		line = append(line, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			break // an unterminated tail is not a committed JSONL record
		}
		if err != nil {
			return PhysicalArtifact{}, err
		}
		complete := bytes.TrimSpace(line)
		if row == 0 {
			artifact.Header, err = DecodeHeader(complete)
		} else {
			var event Event
			event, err = DecodeEvent(complete, uint64(len(artifact.Events)))
			if err == nil && !knownV4EventTypes[event.Type] && !event.Ignorable {
				err = fmt.Errorf("unknown required Session v4 event %q", event.Type)
			}
			if err == nil {
				if event.Type == "session/end-seed" {
					var data map[string]json.RawMessage
					if json.Unmarshal(event.Data, &data) != nil || data == nil {
						err = errors.New("session/end-seed requires object data")
					} else if bytes.Equal(data["inherited"], []byte("true")) {
						cut := event.Seq
						inheritedMarker = &cut
					}
				}
				if err == nil {
					artifact.Events = append(artifact.Events, event)
				}
			}
		}
		if err != nil {
			return PhysicalArtifact{}, fmt.Errorf("Session v4 row %d: %w", row, err)
		}
		row++
		line = line[:0]
	}
	if row == 0 {
		return PhysicalArtifact{}, errors.New("Session v4 log has no complete header")
	}
	if artifact.Header.IsSeeded && inheritedMarker == nil {
		return PhysicalArtifact{}, errors.New("Session v4 seeded header disagrees with inherited end-seed marker")
	}
	if artifact.Header.IsSeeded && inheritedMarker != nil {
		artifact.InheritedEventCount = *inheritedMarker
	}
	return artifact, nil
}
