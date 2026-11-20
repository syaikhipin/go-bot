package hostruntime

import (
	"context"
	"errors"
	"log"
	"time"
)

func (h *NativeHost) CheckScheduleTarget(_ context.Context, id string) error {
	if h.Archived == nil {
		return nil
	}
	archived, err := h.Archived(id)
	if err != nil {
		return err
	}
	if archived {
		return ErrHostSessionArchived
	}
	return nil
}

// ArchiveSession commits the tombstone before releasing the Schedule and
// activation guards. The stopped runtime's journal remains on disk.
func (h *NativeHost) ArchiveSession(ctx context.Context, id string) error {
	return h.ArchiveSessionWithActivity(ctx, id, false)
}

func (h *NativeHost) ArchiveSessionWithActivity(ctx context.Context, id string, stopActivity bool) error {
	if h.SaveArchive == nil {
		return errors.New("session archive storage is unavailable")
	}
	if err := h.CheckScheduleTarget(ctx, id); errors.Is(err, ErrHostSessionArchived) {
		return nil
	} else if err != nil {
		return err
	}
	var loaded bool
	commit := func() error {
		h.operationMu.Lock()
		defer h.operationMu.Unlock()
		h.mu.Lock()
		pending := h.operations[id] != nil
		closed := h.closed
		h.mu.Unlock()
		if closed {
			return ErrHostClosed
		}
		if pending {
			return ErrHostSessionBusy
		}
		if err := h.CheckScheduleTarget(ctx, id); errors.Is(err, ErrHostSessionArchived) {
			return nil
		} else if err != nil {
			return err
		}
		runtime, ok := h.Session(id)
		loaded = ok
		if ok && !stopActivity {
			activity, err := runtime.Activity()
			if err != nil {
				return err
			}
			if !activity.Idle {
				return ErrHostSessionBusy
			}
		}
		if err := h.SaveArchive(ctx, id); err != nil {
			return err
		}
		if ok && stopActivity {
			runtime.Abort()
		}
		return nil
	}
	var err error
	if h.Schedule == nil {
		err = commit()
	} else {
		err = h.Schedule.ArchiveSession(ctx, id, stopActivity, commit)
	}
	if err != nil {
		return err
	}
	if loaded {
		stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := h.StopSession(stop, id); err != nil && !errors.Is(err, ErrHostSessionAbsent) {
			log.Printf("could not stop archived Session %s: %v", id, err)
		}
	}
	return nil
}
