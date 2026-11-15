package session

import (
	"encoding/json"
	"fmt"
)

// ValidateExecutionTrace checks the core AgentLoop event relationships before
// recovery can synthesize closers. Extension events are deliberately opaque;
// their owning subsystem validates their own payload and ordering.
// An open final turn/step is valid here because RepairInterrupted closes it.
func ValidateExecutionTrace(events []Event) error {
	var openTurn, openStep uint64
	nextTurn := uint64(1)
	nextStep := uint64(1)
	pending := map[string]pendingCall{}
	seen := map[string]bool{}
	inbox := map[string]string{}
	inboxRecords := map[string]InboxRecord{}
	turnIDs := map[string]bool{}
	startedMessages := map[string]uint64{}
	cancelledMessages := map[string]bool{}
	var activeMessageID string
	var activeInbox InboxRecord
	var enteredStep bool
	for index, event := range events {
		if event.Seq != uint64(index+1) {
			return fmt.Errorf("execution trace event %d has noncontiguous sequence", index+1)
		}
		invalid := func(reason string) error {
			return fmt.Errorf("execution trace event %d (%s): %s", event.Seq, event.Type, reason)
		}
		requireStep := func() error {
			if openTurn == 0 || openStep == 0 || event.Turn != openTurn || event.Step != openStep {
				return invalid("event is outside its open turn and step")
			}
			return nil
		}
		switch event.Type {
		case "user/inbox", "agent/inbox":
			var input InboxRecord
			if err := json.Unmarshal(event.Data, &input); err != nil || input.MessageID == "" || input.TurnID == "" || inbox[input.MessageID] != "" || turnIDs[input.TurnID] {
				return invalid("invalid or repeated durable inbox identity")
			}
			if (event.Type == "user/inbox" && (input.SourceKind != "" || input.SourceID != "" || input.GoalID != "" || input.GoalRevision != 0 || input.GoalRound != 0)) || (event.Type == "agent/inbox" && !validInternalSource(input)) {
				return invalid("invalid durable inbox provenance")
			}
			inbox[input.MessageID] = input.TurnID
			inboxRecords[input.MessageID] = input
			turnIDs[input.TurnID] = true
		case "turn/start":
			if openTurn != 0 || event.Turn != nextTurn || event.Step != 0 {
				return invalid("expected the next turn with no turn already open")
			}
			var source struct {
				MessageID string `json:"messageId"`
				TurnID    string `json:"turnId"`
			}
			if err := json.Unmarshal(event.Data, &source); err != nil {
				return invalid("invalid turn source")
			}
			if (source.MessageID == "") != (source.TurnID == "") {
				return invalid("turn source has incomplete inbox identity")
			}
			if source.MessageID != "" {
				if inbox[source.MessageID] == "" || inbox[source.MessageID] != source.TurnID || cancelledMessages[source.MessageID] || startedMessages[source.MessageID] != 0 {
					return invalid("turn source is not a pending inbox message")
				}
				startedMessages[source.MessageID] = event.Turn
			}
			activeMessageID = source.MessageID
			activeInbox = inboxRecords[source.MessageID]
			enteredStep = false
			openTurn, openStep, nextStep = event.Turn, 0, 1
			pending = map[string]pendingCall{}
			seen = map[string]bool{}
		case "turn/end":
			if openTurn == 0 || openStep != 0 || event.Turn != openTurn || event.Step != 0 {
				return invalid("turn end does not close the current turn")
			}
			var end struct {
				Reason string `json:"reason"`
			}
			if len(event.Data) > 0 {
				if err := json.Unmarshal(event.Data, &end); err != nil {
					return invalid("invalid turn end payload")
				}
			}
			if end.Reason == TurnInterruptedBeforeStep {
				if activeMessageID == "" || enteredStep || cancelledMessages[activeMessageID] {
					return invalid("inbox requeue is not before the first step")
				}
				startedMessages[activeMessageID] = 0
			}
			activeMessageID = ""
			activeInbox = InboxRecord{}
			openTurn = 0
			nextTurn++
		case "turn/cancel":
			var intent struct {
				MessageID string `json:"messageId"`
				TurnID    string `json:"turnId"`
			}
			if err := json.Unmarshal(event.Data, &intent); err != nil || intent.MessageID == "" || inbox[intent.MessageID] != intent.TurnID || cancelledMessages[intent.MessageID] {
				return invalid("cancellation does not identify one pending inbox message")
			}
			started := startedMessages[intent.MessageID]
			if event.Turn != started || (started != 0 && openTurn != started) || event.Step != 0 {
				return invalid("cancellation is outside its queued or running turn")
			}
			cancelledMessages[intent.MessageID] = true
		case "user/message":
			if openTurn == 0 || event.Turn != openTurn || (event.Step != 0 && event.Step != openStep) {
				return invalid("user message is outside its open turn")
			}
			var message struct {
				Role          string         `json:"role"`
				Content       string         `json:"content"`
				ContentBlocks []ContentBlock `json:"contentBlocks"`
				Source        *struct {
					Kind     string `json:"kind"`
					ID       string `json:"id"`
					GoalID   string `json:"goalId"`
					Revision int    `json:"revision"`
					Round    int    `json:"round"`
				} `json:"source"`
			}
			if err := json.Unmarshal(event.Data, &message); err != nil || message.Role != "user" || (message.Content == "" && len(message.ContentBlocks) == 0) || ValidateDurableContentBlocks(message.ContentBlocks) != nil {
				return invalid("user message is not a model-projectable user object")
			}
			if message.Source != nil && (message.Source.Kind == "goal" || message.Source.Kind == "inbox" || message.Source.Kind == "job") {
				if event.Step != 0 || activeInbox.MessageID == "" {
					return invalid("user message claims an inbox source outside admission")
				}
				if message.Source.Kind == "goal" {
					if activeInbox.SourceKind != "goal" || message.Source.ID != activeInbox.SourceID || message.Source.GoalID != activeInbox.GoalID || message.Source.Revision != activeInbox.GoalRevision || message.Source.Round != activeInbox.GoalRound {
						return invalid("goal message source does not match admitted inbox")
					}
				} else if message.Source.Kind == "job" {
					if activeInbox.SourceKind != "job" || message.Source.ID != activeInbox.SourceID {
						return invalid("job message source does not match admitted inbox")
					}
				} else if activeInbox.SourceKind != "" || message.Source.ID != activeInbox.MessageID {
					return invalid("human message source does not match admitted inbox")
				}
			}
		case "step/start":
			if openTurn == 0 || openStep != 0 || event.Turn != openTurn || event.Step != nextStep {
				return invalid("step start is out of order")
			}
			openStep = event.Step
			enteredStep = true
		case "compaction/start":
			if openTurn != 0 {
				enteredStep = true
			}
			// Compaction is an auxiliary model transaction, not a step boundary.
			// In particular it cannot erase unresolved tool calls from this step.
		case "step/end":
			if err := requireStep(); err != nil {
				return err
			}
			if len(pending) != 0 {
				return invalid("step ended with tool calls lacking results")
			}
			openStep = 0
			nextStep++
		case "model/request", "model/failure":
			if err := requireStep(); err != nil {
				return err
			}
		case "model/retry":
			if err := requireStep(); err != nil {
				return err
			}
			var decision struct {
				FailedAttemptID string `json:"failedAttemptId"`
				ErrorCode       string `json:"errorCode"`
				RetryNumber     int    `json:"retryNumber"`
				DelayMillis     int64  `json:"delayMillis"`
			}
			if index == 0 || events[index-1].Type != "model/failure" || json.Unmarshal(event.Data, &decision) != nil || decision.FailedAttemptID == "" || decision.ErrorCode == "" || decision.RetryNumber < 1 || decision.DelayMillis < 0 {
				return invalid("model retry does not follow a valid failed attempt")
			}
			var failure struct {
				AttemptID string `json:"attemptId"`
			}
			if json.Unmarshal(events[index-1].Data, &failure) != nil || failure.AttemptID != decision.FailedAttemptID {
				return invalid("model retry references a different failed attempt")
			}
		case "assistant/message":
			if err := requireStep(); err != nil {
				return err
			}
			var message AssistantMessage
			if err := json.Unmarshal(event.Data, &message); err != nil {
				return invalid("invalid assistant message")
			}
			for _, call := range message.ToolCalls {
				if call.ID == "" || call.Name == "" || seen[call.ID] {
					return invalid("missing or duplicate assistant tool call identity")
				}
				seen[call.ID] = true
				pending[call.ID] = pendingCall{request: call, step: event.Step}
			}
		case "tool/call":
			if err := requireStep(); err != nil {
				return err
			}
			var call ToolCall
			if err := json.Unmarshal(event.Data, &call); err != nil {
				return invalid("invalid tool call")
			}
			entry, exists := pending[call.ID]
			if !exists || entry.started || call.Name != entry.request.Name || call.Arguments != entry.request.Arguments {
				return invalid("tool call does not match the assistant request or was started twice")
			}
			entry.started = true
			pending[call.ID] = entry
		case "tool/result":
			if err := requireStep(); err != nil {
				return err
			}
			var result ToolResult
			if err := json.Unmarshal(event.Data, &result); err != nil {
				return invalid("invalid tool result")
			}
			if err := ValidateDurableContentBlocks(result.ContentBlocks); err != nil {
				return invalid("invalid tool result content blocks")
			}
			entry, exists := pending[result.ID]
			if !exists || result.Name != entry.request.Name || (!entry.started && (!result.IsError || result.Code != ToolNotStarted)) || (entry.started && result.Code == ToolNotStarted) {
				return invalid("tool result does not match a pending call lifecycle")
			}
			delete(pending, result.ID)
		}
	}
	return nil
}
