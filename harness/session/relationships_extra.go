package sessionformat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

type v4Compaction struct {
	id         string
	command    json.RawMessage
	turn       uint64 // zero represents the released format's null owner
	seq        uint64
	summarized bool
}

func (s *v4Relations) requireCompactionOwner(data map[string]json.RawMessage) error {
	id, err := requiredString(data["compactionId"])
	if err != nil || s.compaction == nil || s.compaction.id != id || !equalJSON(s.compaction.command, data["sourceCommandId"]) {
		return errors.New("event has no matching compaction/start")
	}
	return nil
}

func (s *v4Relations) compact(event Event, data map[string]json.RawMessage) error {
	if event.Type == "compaction/prune" || event.Type == "compaction/summary" {
		if err := s.compactionSpan(event, data); err != nil {
			return err
		}
	}
	if event.Type == "compaction/prune" {
		return nil
	}
	if event.Type == "compaction/start" {
		if s.compaction != nil {
			return errors.New("compaction/start overlaps an open compaction")
		}
		owner, err := optionalOwnerTurn(data["turn"])
		if err != nil || owner != s.turn {
			return errors.New("compaction/start does not match the open turn")
		}
		id, err := requiredString(data["compactionId"])
		if err != nil {
			return err
		}
		s.compaction = &v4Compaction{id: id, command: append(json.RawMessage(nil), data["sourceCommandId"]...), turn: owner, seq: event.Seq}
		return nil
	}
	if err := s.requireCompactionOwner(data); err != nil {
		return err
	}
	if s.compaction.turn != s.turn {
		return errors.New("compaction owner turn changed")
	}
	if event.Type == "compaction/summary" {
		if s.compaction.summarized {
			return errors.New("compaction/summary repeats")
		}
		s.compaction.summarized = true
		return nil
	}
	owner, err := optionalOwnerTurn(data["turn"])
	if err != nil || owner != s.compaction.turn {
		return errors.New("compaction/end changes its owner turn")
	}
	if len(data["error"]) == 0 && !s.compaction.summarized {
		return errors.New("successful compaction/end requires one summary")
	}
	s.compaction = nil
	return nil
}

func (s *v4Relations) compactionSpan(event Event, data map[string]json.RawMessage) error {
	rangeValue, err := objectValue(data["shadowedRange"])
	if err != nil {
		return errors.New("compaction requires shadowedRange")
	}
	start, startErr := earlierSeq(rangeValue["start"], event.Seq)
	end, endErr := earlierSeq(rangeValue["end"], event.Seq)
	first, last := nodeIndex(s.surface, start), nodeIndex(s.surface, end)
	var declared []json.RawMessage
	if startErr != nil || endErr != nil || first < 0 || last < first || json.Unmarshal(data["shadowedSeqs"], &declared) != nil || declared == nil || len(declared) != last-first+1 {
		return errors.New("compaction shadowedSeqs do not name an exact current surface span")
	}
	for index, raw := range declared {
		seq, err := safeCount(raw)
		if err != nil || seq != s.surface[first+index] {
			return errors.New("compaction shadowedSeqs do not name an exact current surface span")
		}
		if s.protectedHead != nil && seq == *s.protectedHead {
			return errors.New("compaction cannot shadow the protected system head")
		}
	}
	return nil
}

func (s *v4Relations) dispatch(event Event, data map[string]json.RawMessage) error {
	if s.turn == 0 {
		return errors.New("PTC dispatch is outside an open turn")
	}
	id, idErr := requiredString(data["subCallId"])
	root, rootErr := requiredString(data["rootCallId"])
	parent, parentErr := requiredString(data["parentCallId"])
	if idErr != nil || rootErr != nil || parentErr != nil {
		return errors.New("PTC dispatch requires subCallId, rootCallId and parentCallId")
	}
	existing, exists := s.dispatches[id]
	if exists && !equalJSON(existing.data["rootCallId"], data["rootCallId"]) {
		return errors.New("PTC dispatch changes rootCallId")
	}
	if parent != root {
		owner, found := s.dispatches[parent]
		if !found || !equalJSON(owner.data["rootCallId"], data["rootCallId"]) {
			return errors.New("PTC parentCallId does not belong to rootCallId")
		}
	}
	if event.Type == "tool/ptc-dispatch-start" {
		if exists {
			return errors.New("PTC dispatch repeats subCallId")
		}
		s.dispatches[id] = v4Dispatch{data: data}
		return nil
	}
	if !exists || existing.settled {
		return errors.New("PTC dispatch has no unique start")
	}
	for _, key := range []string{"rootCallId", "parentCallId", "name", "arguments"} {
		if !equalJSON(existing.data[key], data[key]) {
			return errors.New("PTC dispatch does not match its start")
		}
	}
	existing.settled = true
	s.dispatches[id] = existing
	return nil
}

func (s *v4Relations) retry(event Event, data map[string]json.RawMessage) error {
	id, err := requiredString(data["retryId"])
	attempt, countErr := safeCount(data["retry"])
	if err != nil || countErr != nil {
		return errors.New("llm/retry requires retryId and retry count")
	}
	if event.Type == "llm/retry-started" {
		for _, scheduled := range s.retries {
			if !equalJSON(scheduled["retryId"], data["retryId"]) || !equalJSON(scheduled["retry"], data["retry"]) {
				continue
			}
			if !equalJSON(scheduled["turn"], data["turn"]) || !equalJSON(scheduled["step"], data["step"]) {
				return errors.New("llm/retry-started changes scheduled coordinates")
			}
			key := fmt.Sprintf("%s:%d", id, attempt)
			if s.startedRetries[key] {
				return errors.New("llm/retry-started repeats one scheduled attempt")
			}
			s.startedRetries[key] = true
			return nil
		}
		return errors.New("llm/retry-started pairs no scheduled attempt")
	}
	turn, turnErr := safeCount(data["turn"])
	step, stepErr := safeCount(data["step"])
	expectedStep := s.step
	if expectedStep == 0 {
		expectedStep = s.nextStep - 1
	}
	if turnErr != nil || stepErr != nil || s.turn == 0 || turn != s.turn || step != expectedStep {
		return errors.New("llm/retry does not match the current turn and step")
	}
	if !equalJSON(data["provider"], s.provider) {
		return errors.New("llm/retry provider does not match request/header")
	}
	var prior map[string]json.RawMessage
	for i := len(s.retries) - 1; i >= 0; i-- {
		candidate := s.retries[i]
		if equalJSON(candidate["turn"], data["turn"]) && equalJSON(candidate["step"], data["step"]) && equalJSON(candidate["provider"], data["provider"]) && equalJSON(candidate["policyKey"], data["policyKey"]) {
			prior = candidate
			break
		}
	}
	if prior == nil {
		if attempt != 1 {
			return errors.New("llm/retry skips the first policy attempt")
		}
		for _, other := range s.retries {
			if equalJSON(other["retryId"], data["retryId"]) {
				return errors.New("llm/retry reuses a retryId across policy chains")
			}
		}
	} else {
		last, err := safeCount(prior["retry"])
		if err != nil || attempt != last+1 || !equalJSON(prior["retryId"], data["retryId"]) {
			return errors.New("llm/retry skips or changes its policy chain")
		}
	}
	s.retries = append(s.retries, data)
	return nil
}

func (s *v4Relations) developer(event Event, data map[string]json.RawMessage) error {
	message, err := objectValue(data["message"])
	if err != nil {
		return errors.New("developer/message requires message object")
	}
	var blocks []json.RawMessage
	if json.Unmarshal(message["content"], &blocks) != nil || blocks == nil {
		return errors.New("developer/message requires content array")
	}
	var additions []string
	for _, raw := range blocks {
		block, err := objectValue(raw)
		if err != nil {
			return err
		}
		if bytes.Equal(block["type"], []byte(`"tool-addition"`)) || bytes.Equal(block["type"], []byte(`"tool-removal"`)) {
			name, err := requiredString(block["toolName"])
			if err != nil {
				return errors.New("developer tool change requires toolName")
			}
			if bytes.Equal(block["type"], []byte(`"tool-addition"`)) {
				if _, present := block["tool"]; present {
					return errors.New("developer tool-addition must omit inline definition")
				}
				additions = append(additions, name)
			}
		}
	}
	rawHeaderSeq, hasHeaderSeq := data["headerSeq"]
	if (len(additions) > 0) != hasHeaderSeq {
		return errors.New("developer/message requires headerSeq exactly for tool additions")
	}
	if !hasHeaderSeq {
		return nil
	}
	seq, err := earlierSeq(rawHeaderSeq, event.Seq)
	if err != nil || s.events[seq].Type != "request/header" {
		return errors.New("developer/message headerSeq must reference an earlier request/header")
	}
	headerEvent, err := objectData(s.events[seq])
	if err != nil {
		return err
	}
	header, err := objectValue(headerEvent["header"])
	if err != nil {
		return err
	}
	var schemas []json.RawMessage
	if json.Unmarshal(header["tools"], &schemas) != nil {
		return errors.New("developer/message headerSeq has no tools array")
	}
	for _, name := range additions {
		matches := 0
		for _, raw := range schemas {
			schema, err := objectValue(raw)
			if err != nil || !equalJSON(schema["name"], jsonString(name)) {
				continue
			}
			if _, err := stringValue(schema["description"]); err != nil {
				return errors.New("developer tool addition requires a complete description")
			}
			if _, err := objectValue(schema["parameters"]); err != nil {
				return errors.New("developer tool addition requires parameters object")
			}
			matches++
		}
		if matches != 1 {
			return fmt.Errorf("developer tool-addition %q must name exactly one header tool", name)
		}
	}
	return nil
}

func (s *v4Relations) title(event Event, data map[string]json.RawMessage) error {
	var rawSeqs []json.RawMessage
	if json.Unmarshal(data["messageSeqs"], &rawSeqs) != nil || rawSeqs == nil {
		return errors.New("title messageSeqs must be an array")
	}
	if event.Type == "session/title" {
		source, err := objectValue(data["source"])
		if err != nil {
			return err
		}
		fromUser := bytes.Equal(source["kind"], []byte(`"user"`))
		if (len(rawSeqs) == 0) != fromUser {
			return errors.New("session/title messageSeqs must be empty exactly for a user title")
		}
	}
	seen := make(map[uint64]bool, len(rawSeqs))
	for _, raw := range rawSeqs {
		seq, err := earlierSeq(raw, event.Seq)
		if err != nil || seen[seq] || s.events[seq].Type != "user/message" {
			return errors.New("title messageSeqs must cite distinct earlier human messages")
		}
		user, err := objectData(s.events[seq])
		if err != nil {
			return err
		}
		source, err := objectValue(user["source"])
		if err != nil || !bytes.Equal(source["kind"], []byte(`"user"`)) {
			return errors.New("title messageSeqs must cite human user/message events")
		}
		seen[seq] = true
	}
	if event.Type != "session/title-llm-request" {
		return nil
	}
	var messages []json.RawMessage
	if len(rawSeqs) == 0 || json.Unmarshal(data["messages"], &messages) != nil || len(messages) != 1 {
		return errors.New("session/title-llm-request requires one user message")
	}
	message, err := objectValue(messages[0])
	if err != nil || !bytes.Equal(message["role"], []byte(`"user"`)) {
		return errors.New("session/title-llm-request requires one user message")
	}
	source, err := objectValue(message["source"])
	if err != nil || !bytes.Equal(source["kind"], []byte(`"dsh-session-title-llm"`)) {
		return errors.New("session/title-llm-request has wrong message source")
	}
	var blocks []json.RawMessage
	if json.Unmarshal(message["content"], &blocks) != nil || len(blocks) != 1 {
		return errors.New("session/title-llm-request requires one text block")
	}
	block, err := objectValue(blocks[0])
	if err != nil || !bytes.Equal(block["type"], []byte(`"text"`)) {
		return errors.New("session/title-llm-request requires one text block")
	}
	return nil
}

func jsonString(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

type v4Dispatch struct {
	data    map[string]json.RawMessage
	settled bool
}

type v4Relations struct {
	events         []Event
	turn, step     uint64
	nextStep       uint64
	provider       json.RawMessage
	surface        []uint64
	protectedHead  *uint64
	compaction     *v4Compaction
	orphanCompacts map[uint64]bool
	dispatches     map[string]v4Dispatch
	retries        []map[string]json.RawMessage
	startedRetries map[string]bool
	commands       map[string]bool
}

// ValidateV4Relationships combines the model-visible surface and core
// turn/tool lifecycle with the released v4 compaction, retry, PTC, command,
// title, and developer-header ownership checks. It does not replace complete
// native payload admission or the production Session.fromRestore equivalent.
func ValidateV4Relationships(events []Event) error {
	if err := ValidateCoreRelationships(events); err != nil {
		return err
	}
	if _, err := FoldSurface(events); err != nil {
		return err
	}
	state := v4Relations{
		events: events, nextStep: 1,
		orphanCompacts: make(map[uint64]bool), dispatches: make(map[string]v4Dispatch),
		startedRetries: make(map[string]bool), commands: make(map[string]bool),
	}
	var openCompaction *uint64
	for _, event := range events {
		switch event.Type {
		case "compaction/start":
			seq := event.Seq
			openCompaction = &seq
		case "compaction/end":
			openCompaction = nil
		case "session/end-seed":
			if openCompaction != nil {
				state.orphanCompacts[*openCompaction] = true
			}
			openCompaction = nil
		}
	}
	for _, event := range events {
		if err := state.accept(event); err != nil {
			return fmt.Errorf("%s at seq %d: %w", event.Type, event.Seq, err)
		}
	}
	return nil
}

func (s *v4Relations) accept(event Event) error {
	if !knownV4EventTypes[event.Type] {
		return nil
	}
	if !relationshipType(event.Type) {
		return nil
	}
	data, err := objectData(event)
	if err != nil {
		return err
	}
	if (event.Type == "turn/start" || event.Type == "turn/end") && s.compaction != nil && !s.orphanCompacts[s.compaction.seq] {
		return errors.New("turn boundary crosses an open compaction")
	}
	if err := s.foldSurface(event); err != nil {
		return err
	}
	switch event.Type {
	case "turn/start":
		s.turn, _ = safeCount(data["turn"]) // core validator already proved the coordinate
		s.nextStep = 1
	case "turn/end":
		s.turn = 0
	case "step/start":
		s.step, _ = safeCount(data["step"])
	case "step/end":
		s.nextStep = s.step + 1
		s.step = 0
	case "request/header":
		header, err := objectValue(data["header"])
		if err != nil {
			return errors.New("request/header requires header object")
		}
		config, err := objectValue(header["config"])
		if err != nil {
			return errors.New("request/header requires config object")
		}
		s.provider = append(json.RawMessage(nil), config["provider"]...)
	case "developer/message":
		return s.developer(event, data)
	case "tool/ptc-dispatch-start", "tool/ptc-dispatch":
		return s.dispatch(event, data)
	case "llm/retry", "llm/retry-started":
		return s.retry(event, data)
	case "compaction/start", "compaction/summary", "compaction/end", "compaction/prune":
		return s.compact(event, data)
	case "user/message":
		if isSurfaceReplace(event.SurfaceOp) {
			source, err := objectValue(data["source"])
			if err != nil {
				return err
			}
			if bytes.Equal(source["kind"], []byte(`"compact-checkpoint"`)) {
				return s.requireCompactionOwner(source)
			}
		}
	case "session/title", "session/title-llm-request":
		return s.title(event, data)
	case "command/run":
		id, err := requiredString(data["commandId"])
		if err != nil || s.commands[id] {
			return errors.New("command/run repeats or omits commandId")
		}
		s.commands[id] = true
	case "command/done":
		id, err := requiredString(data["commandId"])
		if err != nil || !s.commands[id] {
			return errors.New("command/done has no prior command/run")
		}
		if raw := data["sourceEventSeq"]; len(raw) != 0 {
			seq, err := earlierSeq(raw, event.Seq)
			if err != nil || !bytes.Equal(data["kind"], []byte(`"success"`)) || s.events[seq].Type == "command/run" || s.events[seq].Type == "command/done" {
				return errors.New("command/done has invalid sourceEventSeq")
			}
		}
	case "session/end-seed":
		s.compaction = nil
	}
	return nil
}

func relationshipType(name string) bool {
	if surfaceEligible(name) {
		return true
	}
	switch name {
	case "turn/start", "turn/end", "step/start", "step/end", "assistant/attempt", "tool/call",
		"request/header", "request/context", "tool/ptc-dispatch-start", "tool/ptc-dispatch",
		"llm/retry", "llm/retry-started", "session/title", "session/title-llm-request",
		"command/run", "command/done", "compaction/start", "compaction/summary",
		"compaction/end", "compaction/prune", "session/end-seed":
		return true
	default:
		return false
	}
}

func (s *v4Relations) foldSurface(event Event) error {
	if !surfaceEligible(event.Type) {
		return nil
	}
	if event.Type == "system/message" && len(s.surface) > 0 && s.protectedHead == nil {
		return errors.New("system/message requires a protected first surface head")
	}
	if !isSurfaceReplace(event.SurfaceOp) {
		if event.Type == "system/message" && len(s.surface) == 0 {
			seq := event.Seq
			s.protectedHead = &seq
		}
		s.surface = append(s.surface, event.Seq)
		return nil
	}
	var op struct {
		StartSeq uint64 `json:"startSeq"`
		EndSeq   uint64 `json:"endSeq"`
	}
	if json.Unmarshal(event.SurfaceOp, &op) != nil {
		return errors.New("invalid surface replacement")
	}
	first, last := nodeIndex(s.surface, op.StartSeq), nodeIndex(s.surface, op.EndSeq)
	if first < 0 || last < first {
		return errors.New("surface replacement range is not current")
	}
	shadowed := s.surface[first : last+1]
	if s.protectedHead != nil {
		for _, seq := range shadowed {
			if seq == *s.protectedHead {
				if event.Type != "system/message" || len(shadowed) != 1 {
					return errors.New("surface replacement shadows the protected system head")
				}
				replacement := event.Seq
				s.protectedHead = &replacement
			}
		}
	}
	next := make([]uint64, 0, len(s.surface)-(last-first))
	next = append(next, s.surface[:first]...)
	next = append(next, event.Seq)
	next = append(next, s.surface[last+1:]...)
	s.surface = next
	return nil
}

func earlierSeq(raw json.RawMessage, current uint64) (uint64, error) {
	seq, err := safeCount(raw)
	if err != nil || seq >= current {
		return 0, errors.New("event reference must name an earlier seq")
	}
	return seq, nil
}

func optionalOwnerTurn(raw json.RawMessage) (uint64, error) {
	if bytes.Equal(raw, []byte("null")) {
		return 0, nil
	}
	turn, err := safeCount(raw)
	if err != nil || turn == 0 {
		return 0, errors.New("compaction turn must be null or positive")
	}
	return turn, nil
}

func equalJSON(left, right json.RawMessage) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == len(right)
	}
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}
