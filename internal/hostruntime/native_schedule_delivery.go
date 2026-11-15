package hostruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/google/uuid"

	"go-harness/harness/schedule"
	"go-harness/harness/session"
)

// ValidateScheduleDestination checks a stored top-level Session without
// activating it. Reserved new Sessions are validated when they are created.
func (h *NativeHost) ValidateScheduleDestination(ctx context.Context, id string) error {
	if err := h.CheckScheduleTarget(ctx, id); err != nil {
		return err
	}
	if h.ReadSession == nil {
		return os.ErrNotExist
	}
	if h.ReadSessionHeader != nil {
		header, err := h.ReadSessionHeader(ctx, id)
		if err != nil {
			return err
		}
		if header.Origin == "subagent" || header.ParentSession != "" {
			return schedule.ErrInvalid
		}
		return nil
	}
	artifact, err := h.ReadSession(ctx, id)
	if err != nil {
		return err
	}
	if artifact.Header.Origin == "subagent" || artifact.Header.ParentSession != "" {
		return schedule.ErrInvalid
	}
	return nil
}

func (h *NativeHost) ObserveScheduleExecution(ctx context.Context, receipt schedule.Receipt, recovering bool) (bool, error) {
	if runtime, loaded := h.Session(receipt.TargetSessionID); loaded {
		return runtime.scheduleExecution(receipt.MessageID, recovering)
	}
	if !recovering || h.ReadSession == nil {
		return false, nil
	}
	if err := h.CheckScheduleTarget(ctx, receipt.TargetSessionID); errors.Is(err, ErrHostSessionArchived) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	artifact, err := h.ReadSession(ctx, receipt.TargetSessionID)
	if err != nil {
		return false, err
	}
	inbox, err := sessionformat.FoldInbox(artifact.Events)
	if err != nil || !hasScheduleMessage(inbox, receipt.MessageID) {
		return false, err
	}
	// Restore the existing inbox and wake its original message. No new
	// delivery or receipt is written during recovery.
	runtime, err := h.scheduleRuntime(ctx, receipt.TargetSessionID)
	if err != nil {
		return false, err
	}
	return runtime.scheduleExecution(receipt.MessageID, false)
}

func (r *NativeRuntime) scheduleExecution(id string, recovering bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil || r.stopping || r.fatalErr != nil {
		return false, nil
	}
	inbox := r.live.Session.Inbox()
	if recovering {
		if !hasScheduleMessage(inbox, id) {
			return false, nil
		}
		r.kick()
		return true, nil
	}
	// Injected next-step notices do not start a turn. They may remain in an
	// otherwise idle Session and must not hold its scheduling slot.
	return r.activeCancel != nil || r.compactDone != nil || len(inbox.NextTurn) != 0 || hasScheduleMessage(inbox, id), nil
}

func hasScheduleMessage(inbox sessionformat.InboxState, id string) bool {
	for _, messages := range [][]json.RawMessage{inbox.NextTurn, inbox.NextStep} {
		for _, raw := range messages {
			var message struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &message) == nil && message.ID == id {
				return true
			}
		}
	}
	return false
}

func (h *NativeHost) NotifyScheduleChange(ctx context.Context, id, text string) error {
	runtime, loaded := h.Session(id)
	if !loaded {
		return nil
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.live == nil || runtime.stopping || runtime.fatalErr != nil {
		return nil
	}
	message, err := json.Marshal(map[string]any{
		"id": uuid.NewString(), "role": "user", "source": map[string]any{"kind": "schedule"},
		"content": []map[string]any{{"type": "text", "text": text}},
	})
	if err != nil {
		return err
	}
	_, err = runtime.live.Agent.InjectMessage(ctx, message)
	return err
}
