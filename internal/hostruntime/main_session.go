package hostruntime

import (
	"context"
	"errors"
)

var ErrMainSessionArchive = errors.New("main_session_cannot_archive")

func (h *NativeHost) IsMainSession(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return id != "" && h.mainSessionID == id
}

// EnsureMainSession prepares durable history without activating an Agent or
// resolving an LLM. The account's stable identity is provided by Controller.
func (h *NativeHost) EnsureMainSession(ctx context.Context, id, locale string) error {
	h.operationMu.Lock()
	defer h.operationMu.Unlock()
	h.mu.Lock()
	closed, previous := h.closed, h.mainSessionID
	h.mu.Unlock()
	if closed {
		return ErrHostClosed
	}
	if h.PrepareMain == nil || (previous != "" && previous != id) {
		return errors.New("invalid main Session identity")
	}
	if err := h.PrepareMain(ctx, id, locale); err != nil {
		return err
	}
	h.mu.Lock()
	h.mainSessionID = id
	h.mu.Unlock()
	if previous == "" && h.MainReady != nil {
		return h.MainReady(ctx, id)
	}
	return nil
}
