package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReusedScheduleWaitsAndCoalescesAfterAgentIdle(t *testing.T) {
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	var now atomic.Int64
	now.Store(start.UnixMilli())
	var busy atomic.Bool
	var deliveries []string
	service, err := NewHostService(&memoryStore{document: Document{Version: 1}}, noOpHostPrepare,
		func(_ context.Context, sessionID, prompt string) (string, error) {
			busy.Store(true)
			deliveries = append(deliveries, prompt)
			return fmt.Sprintf("%s-%d", sessionID, len(deliveries)), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.UnixMilli(now.Load()) }
	service.ConfigureDelivery(Limits{}, func(_ context.Context, _ Receipt, recovering bool) (bool, error) {
		return !recovering && busy.Load(), nil
	}, nil)
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	every := int64(60)
	entry, err := service.CreateTask(t.Context(), "source", CreateRequest{Title: "Test", Prompt: "Instructions",
		Target: &Target{Kind: "session", SessionID: "source"}, Timing: Timing{Kind: "every", EverySeconds: &every}})
	if err != nil {
		t.Fatal(err)
	}
	now.Store(start.Add(time.Minute).UnixMilli())
	service.dispatchDue(t.Context(), nil)
	first, err := service.ViewTask(entry.Record.ID)
	if err != nil || !first.ExecutionActive || first.LastDelivery == nil {
		t.Fatalf("first admission: %+v, %v", first, err)
	}
	now.Store(start.Add(4*time.Minute + 12*time.Second).UnixMilli())
	service.dispatchDue(t.Context(), nil)
	stillHeld, _ := service.ViewTask(entry.Record.ID)
	if stillHeld.LastDelivery.MessageID != first.LastDelivery.MessageID {
		t.Fatal("a later occurrence was admitted while the Agent was busy")
	}
	if _, err := service.RunNow(t.Context(), entry.Record.ID); !errors.Is(err, ErrExecutionPending) {
		t.Fatalf("manual run while held: %v", err)
	}
	busy.Store(false)
	service.dispatchDue(t.Context(), nil)
	catchup, _ := service.ViewTask(entry.Record.ID)
	page, err := service.History(entry.Record.ID, 10, nil)
	if err != nil || len(page.Records) != 2 || catchup.LastDelivery == nil ||
		!strings.Contains(catchup.LastDelivery.ScheduledAt, "00:04:00") ||
		!strings.Contains(catchup.Record.ScheduledAt, "00:05:00") {
		t.Fatalf("catch-up did not collapse to the latest occurrence: %+v, %+v, %v", catchup, page, err)
	}
}

func TestScheduleRecoveryRecognizesPriorInboxMessage(t *testing.T) {
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	record, err := NewEvery("reminder", "Test", "Instructions", 60, start)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &Receipt{ScheduledAt: "2099-01-01T00:00:00.000Z", DeliveredAt: "2099-01-01T00:00:00.001Z", MessageID: "prior-inbox-id", TargetSessionID: "source"}
	store := &memoryStore{document: Document{Version: 1, Tasks: []Task{{Record: record, Status: "active", SessionID: "source", LastDelivery: receipt}}}}
	var sent atomic.Bool
	service, err := NewHostService(store, noOpHostPrepare, func(context.Context, string, string) (string, error) {
		sent.Store(true)
		return "unexpected", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(start.UnixMilli())
	service.now = func() time.Time { return time.UnixMilli(clock.Load()) }
	service.ConfigureDelivery(Limits{}, func(_ context.Context, got Receipt, _ bool) (bool, error) {
		return got.MessageID == receipt.MessageID, nil
	}, nil)
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	clock.Store(start.Add(3 * time.Minute).UnixMilli())
	service.dispatchDue(t.Context(), nil)
	entry, err := service.ViewTask(record.ID)
	if err != nil || !entry.ExecutionActive || sent.Load() {
		t.Fatalf("restored prior inbox execution was lost or duplicated: %+v, %v", entry, err)
	}
}
