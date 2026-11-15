// Package sessionformat defines the physical JSONL envelope used by the
// current DeepSeek Harness Session format. Payload validation and surface
// reconstruction belong to the Session implementation, not this framing layer.
package sessionformat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
)

const Version = 4
const Filename = "session.v4.jsonl"
const maxSafeInteger int64 = 1<<53 - 1

// Header is the exact set of physical header fields accepted by the current
// DeepSeek JSONL codec. Deployment metadata must not be added here.
type Header struct {
	Type            string `json:"type"`
	Version         int    `json:"version"`
	ID              string `json:"id"`
	CreatedAt       int64  `json:"createdAt"`
	CWD             string `json:"cwd,omitempty"`
	ParentSession   string `json:"parentSession,omitempty"`
	IsSeeded        bool   `json:"isSeeded"`
	Origin          string `json:"origin,omitempty"`
	DelegationDepth int    `json:"delegationDepth"`
	AgentPreset     string `json:"agentPreset,omitempty"`
}

func (h Header) Validate() error {
	if h.Type != "session" || h.Version != Version || h.CreatedAt < 0 || h.CreatedAt > maxSafeInteger || h.DelegationDepth < 0 || int64(h.DelegationDepth) > maxSafeInteger {
		return errors.New("invalid DeepSeek Session v4 header")
	}
	if h.CWD != "" && !filepath.IsAbs(h.CWD) {
		return errors.New("DeepSeek Session cwd must be absolute")
	}
	if h.Origin != "" && h.Origin != "subagent" {
		return errors.New("invalid DeepSeek Session origin")
	}
	return nil
}

// DecodeHeader requires every released-v4 required field and rejects custom
// product fields. A future physical version is not accepted as v4.
func DecodeHeader(line []byte) (Header, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return Header{}, err
	}
	for _, key := range []string{"type", "version", "id", "createdAt", "isSeeded", "delegationDepth"} {
		if value, ok := fields[key]; !ok || bytes.Equal(value, []byte("null")) {
			return Header{}, fmt.Errorf("DeepSeek Session header lacks %s", key)
		}
	}
	for _, key := range []string{"cwd", "parentSession", "origin", "agentPreset"} {
		if value, ok := fields[key]; ok && bytes.Equal(value, []byte("null")) {
			return Header{}, fmt.Errorf("DeepSeek Session header has null %s", key)
		}
	}
	allowed := map[string]bool{"type": true, "version": true, "id": true, "createdAt": true, "cwd": true, "parentSession": true, "isSeeded": true, "origin": true, "delegationDepth": true, "agentPreset": true}
	for key := range fields {
		if !allowed[key] {
			return Header{}, fmt.Errorf("DeepSeek Session header has unexpected field %s", key)
		}
	}
	version, versionErr := safeCount(fields["version"])
	createdAt, createdErr := safeCount(fields["createdAt"])
	depth, depthErr := safeCount(fields["delegationDepth"])
	if versionErr != nil || createdErr != nil || depthErr != nil {
		return Header{}, errors.New("invalid DeepSeek Session header numbers")
	}
	header := Header{Version: int(version), CreatedAt: int64(createdAt), DelegationDepth: int(depth)}
	for _, field := range []struct {
		name  string
		value *string
	}{{"type", &header.Type}, {"id", &header.ID}, {"cwd", &header.CWD}, {"parentSession", &header.ParentSession}, {"origin", &header.Origin}, {"agentPreset", &header.AgentPreset}} {
		if raw, ok := fields[field.name]; ok {
			if err := json.Unmarshal(raw, field.value); err != nil {
				return Header{}, err
			}
		}
	}
	if err := json.Unmarshal(fields["isSeeded"], &header.IsSeeded); err != nil {
		return Header{}, err
	}
	if _, present := fields["cwd"]; present && header.CWD == "" {
		return Header{}, errors.New("DeepSeek Session cwd must be absolute")
	}
	if _, present := fields["origin"]; present && header.Origin != "subagent" {
		return Header{}, errors.New("invalid DeepSeek Session origin")
	}
	return header, header.Validate()
}

func EncodeHeader(header Header) ([]byte, error) {
	if err := header.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(header)
}

// Event is the physical v4 envelope. It deliberately keeps data and surfaceOp
// as JSON so plugin-owned payloads remain lossless. It is not a substitute for
// the current Session event vocabulary and surface validation.
type Event struct {
	Type            string          `json:"type"`
	Seq             uint64          `json:"seq"`
	Time            int64           `json:"time"`
	Data            json.RawMessage `json:"data"`
	SurfaceOp       json.RawMessage `json:"surfaceOp,omitempty"`
	SourceEventSeqs json.RawMessage `json:"sourceEventSeqs,omitempty"`
	Ignorable       bool            `json:"ignorable,omitempty"`
}

func (e Event) ValidateEnvelope(expectedSeq uint64) error {
	if e.Seq != expectedSeq || e.Seq > uint64(maxSafeInteger) || e.Time < -maxSafeInteger || e.Time > maxSafeInteger || len(e.Data) == 0 || !json.Valid(e.Data) {
		return errors.New("invalid DeepSeek Session v4 event envelope")
	}
	if err := e.validateSurface(); err != nil {
		return err
	}
	return nil
}

func (e Event) validateSurface() error {
	surface := e.Type == "system/message" || e.Type == "developer/message" || e.Type == "user/message" || e.Type == "assistant/message" || e.Type == "tool/result"
	if !surface {
		// The vocabulary layer must distinguish a known log-only event from an
		// opaque ignorable extension. This physical layer preserves the latter.
		if e.Ignorable {
			return validateOpaqueSurfaceJSON(e.SurfaceOp, e.SourceEventSeqs, e.Seq)
		}
		if len(e.SurfaceOp) != 0 || len(e.SourceEventSeqs) != 0 {
			return errors.New("log-only DeepSeek Session event cannot carry surface metadata")
		}
		return nil
	}
	if len(e.SurfaceOp) == 0 {
		return errors.New("surface DeepSeek Session event requires surfaceOp")
	}
	var operationName string
	if err := json.Unmarshal(e.SurfaceOp, &operationName); err == nil && operationName == "append" {
		// No endpoint validation is needed for an append.
	} else {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(e.SurfaceOp, &fields); err != nil || len(fields) != 3 {
			return errors.New("invalid DeepSeek Session surface replacement")
		}
		for _, key := range []string{"op", "startSeq", "endSeq"} {
			if _, ok := fields[key]; !ok {
				return errors.New("invalid DeepSeek Session surface replacement")
			}
		}
		var op string
		start, startErr := safeCount(fields["startSeq"])
		end, endErr := safeCount(fields["endSeq"])
		if err := json.Unmarshal(fields["op"], &op); err != nil || op != "replace" || startErr != nil || endErr != nil || start >= e.Seq || end >= e.Seq {
			return errors.New("invalid DeepSeek Session surface replacement")
		}
	}
	if len(e.SourceEventSeqs) == 0 {
		return nil
	}
	if e.Type == "assistant/message" {
		return errors.New("assistant/message embeds its stream and cannot carry sourceEventSeqs")
	}
	return validateSourceRanges(e.SourceEventSeqs, e.Seq)
}

// The released physical codec packs runs of at least three earlier source
// sequences as [start,end]. A row can also contain individual sequence values.
func validateSourceRanges(raw json.RawMessage, eventSeq uint64) error {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return errors.New("invalid DeepSeek Session sourceEventSeqs")
	}
	var count uint64
	var previous uint64
	var havePrevious, increasing, hasRange bool
	increasing = true
	seen := make(map[uint64]struct{}, len(entries))
	for _, entry := range entries {
		start, end, ranged, err := decodeSourceEntry(entry)
		if err != nil || end >= eventSeq || end < start {
			return errors.New("DeepSeek Session sourceEventSeqs must reference earlier events")
		}
		if end-start+1 > eventSeq-count {
			return errors.New("DeepSeek Session sourceEventSeqs range exceeds event seq")
		}
		count += end - start + 1
		if havePrevious && start <= previous {
			increasing = false
		}
		if ranged {
			hasRange = true
		} else {
			if _, duplicate := seen[start]; duplicate {
				return errors.New("DeepSeek Session sourceEventSeqs must be unique")
			}
			seen[start] = struct{}{}
		}
		previous, havePrevious = end, true
	}
	if hasRange && !increasing {
		return errors.New("DeepSeek Session sourceEventSeqs ranges must be strictly increasing")
	}
	return nil
}

// EncodeSourceSeqs mirrors DeepSeek's logical-to-physical v4 boundary: a
// strictly increasing run of at least three consecutive source seqs becomes
// one inclusive [start,end] pair. Unordered unique lists remain individual
// integers so their in-memory order is preserved.
func EncodeSourceSeqs(seqs []uint64, eventSeq uint64) (json.RawMessage, error) {
	if len(seqs) == 0 {
		return nil, errors.New("sourceEventSeqs must not be empty")
	}
	seen := make(map[uint64]struct{}, len(seqs))
	increasing := true
	for index, seq := range seqs {
		if seq >= eventSeq || seq > uint64(maxSafeInteger) {
			return nil, errors.New("sourceEventSeqs must reference earlier events")
		}
		if _, duplicate := seen[seq]; duplicate {
			return nil, errors.New("sourceEventSeqs must be unique")
		}
		seen[seq] = struct{}{}
		if index > 0 && seq <= seqs[index-1] {
			increasing = false
		}
	}
	entries := make([]any, 0, len(seqs))
	if increasing {
		for start := 0; start < len(seqs); {
			end := start
			for end+1 < len(seqs) && seqs[end+1] == seqs[end]+1 {
				end++
			}
			if end-start >= 2 {
				entries = append(entries, [2]uint64{seqs[start], seqs[end]})
			} else {
				for index := start; index <= end; index++ {
					entries = append(entries, seqs[index])
				}
			}
			start = end + 1
		}
	} else {
		for _, seq := range seqs {
			entries = append(entries, seq)
		}
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	return encoded, validateSourceRanges(encoded, eventSeq)
}

func decodeSourceEntry(raw json.RawMessage) (start, end uint64, ranged bool, err error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '[' {
		var pair []json.RawMessage
		if err = json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
			return 0, 0, false, errors.New("sourceEventSeqs range must be a pair")
		}
		start, err = safeCount(pair[0])
		if err != nil {
			return 0, 0, false, err
		}
		end, err = safeCount(pair[1])
		return start, end, true, err
	}
	start, err = safeCount(raw)
	return start, start, false, err
}

func safeCount(raw json.RawMessage) (uint64, error) {
	value, err := safeSignedInteger(raw)
	if err != nil || value < 0 {
		return 0, errors.New("invalid DeepSeek Session sequence number")
	}
	return uint64(value), nil
}

func safeSignedInteger(raw json.RawMessage) (int64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0, errors.New("invalid DeepSeek Session safe integer")
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < -float64(maxSafeInteger) || value > float64(maxSafeInteger) || math.Trunc(value) != value || (value == 0 && math.Signbit(value)) {
		return 0, errors.New("invalid DeepSeek Session safe integer")
	}
	return int64(value), nil
}

func validateOpaqueSurfaceJSON(operation, sources json.RawMessage, eventSeq uint64) error {
	if len(operation) > 0 && !json.Valid(operation) {
		return errors.New("invalid opaque Session surfaceOp JSON")
	}
	if len(sources) > 0 {
		return validateSourceRanges(sources, eventSeq)
	}
	return nil
}

func DecodeEvent(line []byte, expectedSeq uint64) (Event, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return Event{}, err
	}
	for _, key := range []string{"type", "seq", "time", "data"} {
		if value, ok := fields[key]; !ok || (key != "data" && bytes.Equal(value, []byte("null"))) {
			return Event{}, fmt.Errorf("DeepSeek Session event lacks %s", key)
		}
	}
	if value, ok := fields["ignorable"]; ok && !bytes.Equal(value, []byte("true")) {
		return Event{}, errors.New("DeepSeek Session ignorable must be true when present")
	}
	allowed := map[string]bool{"type": true, "seq": true, "time": true, "data": true, "surfaceOp": true, "sourceEventSeqs": true, "ignorable": true}
	for key := range fields {
		if !allowed[key] {
			return Event{}, fmt.Errorf("DeepSeek Session event has unexpected field %s", key)
		}
	}
	seq, seqErr := safeCount(fields["seq"])
	when, timeErr := safeSignedInteger(fields["time"])
	if seqErr != nil || timeErr != nil {
		return Event{}, errors.New("invalid DeepSeek Session event coordinates")
	}
	var event Event
	if err := json.Unmarshal(fields["type"], &event.Type); err != nil {
		return Event{}, err
	}
	event.Seq, event.Time = seq, when
	event.Data = append(json.RawMessage(nil), fields["data"]...)
	event.SurfaceOp = append(json.RawMessage(nil), fields["surfaceOp"]...)
	event.SourceEventSeqs = append(json.RawMessage(nil), fields["sourceEventSeqs"]...)
	event.Ignorable = len(fields["ignorable"]) != 0
	return event, event.ValidateEnvelope(expectedSeq)
}

func EncodeEvent(event Event, expectedSeq uint64) ([]byte, error) {
	if err := event.ValidateEnvelope(expectedSeq); err != nil {
		return nil, err
	}
	return json.Marshal(event)
}
