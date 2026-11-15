package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryStore struct {
	mu       sync.Mutex
	document Document
	failSave bool
	onSave   func()
}

func TestServiceRetriesFailedDueTaskOnAnotherTaskTimer(t *testing.T) {
	now := time.Now().UTC()
	first, err := NewAfter("first", "First", "First reminder", 1, now.Add(-2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAfter("second", "Second", "Second reminder", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{document: Document{Version: 1, Tasks: []Task{
		{SessionID: "same-session", Record: first, Status: "active", DeliveryHistory: History{Records: []Receipt{}}},
		{SessionID: "same-session", Record: second, Status: "active", DeliveryHistory: History{Records: []Receipt{}}},
	}}}
	var firstAttempts atomic.Int32
	delivered := make(chan string, 4)
	service, err := NewHostService(store, noOpHostPrepare, func(_ context.Context, _ string, text string) (string, error) {
		if strings.Contains(text, `schedule_id_json: "first"`) {
			attempt := firstAttempts.Add(1)
			delivered <- "first"
			if attempt == 1 {
				return "", errors.New("first delivery unavailable")
			}
			return "first-delivered", nil
		}
		if strings.Contains(text, `schedule_id_json: "second"`) {
			delivered <- "second"
			return "second-delivered", nil
		}
		return "", errors.New("unexpected reminder")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	select {
	case id := <-delivered:
		if id != "first" {
			t.Fatalf("first due task = %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("initial due task did not run")
	}
	select {
	case id := <-delivered:
		if id != "first" {
			t.Fatalf("failed task was not reconsidered at next timer: %q", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("later task timer did not reconsider the failed task")
	}
	select {
	case id := <-delivered:
		if id != "second" {
			t.Fatalf("next task delivery = %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("later task did not run")
	}
	if got := firstAttempts.Load(); got != 2 {
		t.Fatalf("failed task attempts = %d, want two scans", got)
	}
}

func TestServiceCloseCancelsInFlightDelivery(t *testing.T) {
	ctx := context.Background()
	record, err := NewAfter("closing", "Check", "Wait for cancellation", 1, time.Now().UTC().Add(-2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{document: Document{Version: 1, Tasks: []Task{{
		Record: record, Status: "active", DeliveryHistory: History{Records: []Receipt{}},
	}}}}
	entered := make(chan struct{})
	service, err := NewService(store, func(deliveryCtx context.Context, _ string) (string, error) {
		close(entered)
		<-deliveryCtx.Done()
		return "", deliveryCtx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("due delivery did not start")
	}
	closed := make(chan struct{})
	go func() {
		service.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Schedule Close waited for a delivery before cancelling it")
	}
}

func TestServiceCloseStopsLaterDueDeliveries(t *testing.T) {
	now := time.Now().UTC()
	first, err := NewAfter("first", "First", "First reminder", 1, now.Add(-2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAfter("second", "Second", "Second reminder", 1, now.Add(-2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{document: Document{Version: 1, Tasks: []Task{
		{Record: first, Status: "active", DeliveryHistory: History{Records: []Receipt{}}},
		{Record: second, Status: "active", DeliveryHistory: History{Records: []Receipt{}}},
	}}}
	entered := make(chan struct{})
	var delivered atomic.Int32
	service, err := NewService(store, func(ctx context.Context, _ string) (string, error) {
		if delivered.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
		}
		return "message", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		service.Close()
		t.Fatal("first due delivery did not start")
	}
	service.Close()
	if got := delivered.Load(); got != 1 {
		t.Fatalf("Schedule delivered %d due tasks during shutdown, want only the admitted first task", got)
	}
}

func TestHostScheduleHistoryPagesWithinOriginalSession(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	record, err := NewAfter("history", "Check", "Original prompt", 3600, now)
	if err != nil {
		t.Fatal(err)
	}
	receipts := []Receipt{
		{ScheduledAt: "2026-10-11T10:00:00.000Z", DeliveredAt: "2026-10-11T10:00:01.000Z", MessageID: "message-1", Prompt: "Original prompt"},
		{ScheduledAt: "2026-10-11T11:00:00.000Z", DeliveredAt: "2026-10-11T11:00:01.000Z", MessageID: "message-2", Prompt: "Original prompt"},
		{ScheduledAt: "2026-10-11T12:00:00.000Z", DeliveredAt: "2026-10-11T12:00:01.000Z", MessageID: "message-3", Prompt: "Original prompt"},
	}
	last := receipts[len(receipts)-1]
	last.Prompt = ""
	store := &memoryStore{document: Document{Version: 1, Tasks: []Task{{
		SessionID: "session-a", Record: record, Status: "inactive", LastDelivery: &last,
		DeliveryHistory: History{Records: receipts},
	}}}}
	service, err := NewHostService(store, noOpHostPrepare, func(context.Context, string, string) (string, error) {
		t.Fatal("history must not activate a Session")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	page, err := service.HistoryFor("session-a", "history", 2, nil)
	if err != nil || len(page.Records) != 2 || page.Records[0].MessageID != "message-3" || page.Records[1].MessageID != "message-2" ||
		page.NextBefore != "message-2" || page.Retention.Days != 30 || page.Retention.Records != 200 {
		t.Fatalf("first history page = %+v, %v", page, err)
	}
	page, err = service.HistoryFor("session-a", "history", 2, &page.NextBefore)
	if err != nil || len(page.Records) != 1 || page.Records[0].MessageID != "message-1" || page.NextBefore != "" {
		t.Fatalf("second history page = %+v, %v", page, err)
	}
	wrongSession, err := service.HistoryFor("session-b", "history", 2, nil)
	if err != nil || wrongSession.Code != "schedule_not_found" {
		t.Fatalf("foreign Session history = %+v, %v", wrongSession, err)
	}
	missing := "missing"
	badCursor, err := service.HistoryFor("session-a", "history", 2, &missing)
	if err != nil || badCursor.Code != "delivery_cursor_not_found" {
		t.Fatalf("unknown history cursor = %+v, %v", badCursor, err)
	}
	empty := ""
	badCursor, err = service.HistoryFor("session-a", "history", 2, &empty)
	if err != nil || badCursor.Code != "delivery_cursor_not_found" {
		t.Fatalf("explicit empty history cursor = %+v, %v", badCursor, err)
	}
	if _, err := service.HistoryFor("session-a", "history", 101, nil); err == nil {
		t.Fatal("unbounded history query was accepted")
	}
}

func TestHostScheduleHistoryCursorSurvivesNewDeliveryAndClockRollback(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	record, err := NewAfter("history-many", "Check", "Current prompt", 3600, now)
	if err != nil {
		t.Fatal(err)
	}
	receipts := make([]Receipt, 103)
	for index := range receipts {
		delivered := now.Add(-time.Hour)
		if index%2 != 0 {
			delivered = now.Add(-2 * time.Hour)
		}
		receipts[index] = Receipt{
			ScheduledAt: now.Add(-3 * time.Hour).Format("2006-01-02T15:04:05.000Z"),
			DeliveredAt: delivered.Format("2006-01-02T15:04:05.000Z"),
			MessageID:   fmt.Sprintf("message-%d", index),
			Prompt:      fmt.Sprintf("Older prompt %d", index),
		}
	}
	last := receipts[len(receipts)-1]
	last.Prompt = ""
	store := &memoryStore{document: Document{Version: 1, Tasks: []Task{{
		SessionID: "history-owner", Record: record, Status: "active", LastDelivery: &last,
		DeliveryHistory: History{Records: receipts},
	}}}}
	var deliveries int
	service, err := NewHostService(store, noOpHostPrepare, func(_ context.Context, sessionID, _ string) (string, error) {
		if sessionID != "history-owner" {
			t.Fatalf("delivery changed Session binding: %q", sessionID)
		}
		deliveries++
		return "message-103", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	first, err := service.HistoryFor("history-owner", record.ID, 100, nil)
	if err != nil || len(first.Records) != 100 || first.Records[0].MessageID != "message-102" ||
		first.Records[99].MessageID != "message-3" || first.NextBefore != "message-3" {
		t.Fatalf("first history page = %+v, %v", first, err)
	}
	// The original 103 receipts must remain addressable by identity even if a
	// new occurrence is accepted between page reads. Wall-clock order is not a
	// valid cursor because the older receipts alternate deliveredAt values.
	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	if failed := service.dispatchDue(ctx, nil); len(failed) != 0 || deliveries != 1 {
		t.Fatalf("new delivery failed=%v count=%d", failed, deliveries)
	}
	second, err := service.HistoryFor("history-owner", record.ID, 100, &first.NextBefore)
	if err != nil || len(second.Records) != 3 || second.Records[0].MessageID != "message-2" ||
		second.Records[2].MessageID != "message-0" || second.NextBefore != "" {
		t.Fatalf("second history page = %+v, %v", second, err)
	}
	fresh, err := service.HistoryFor("history-owner", record.ID, 1, nil)
	if err != nil || len(fresh.Records) != 1 || fresh.Records[0].MessageID != "message-103" {
		t.Fatalf("fresh history page = %+v, %v", fresh, err)
	}
	first.Records[0].Prompt = "mutated response copy"
	first.Records = nil
	again, err := service.HistoryFor("history-owner", record.ID, 100, nil)
	if err != nil || len(again.Records) != 100 || again.Records[1].MessageID != "message-102" ||
		again.Records[1].Prompt != "Older prompt 102" {
		t.Fatalf("history response mutated storage: %+v, %v", again, err)
	}
}

func (m *memoryStore) Load(context.Context) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, _ := json.Marshal(m.document)
	var result Document
	_ = json.Unmarshal(data, &result)
	return result, nil
}

func (m *memoryStore) Save(_ context.Context, document Document) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSave {
		return errors.New("storage unavailable")
	}
	data, _ := json.Marshal(document)
	if err := json.Unmarshal(data, &m.document); err != nil {
		return err
	}
	if m.onSave != nil {
		m.onSave()
	}
	return nil
}

func TestServiceDeliversOneShotBeforeCommittingReceipt(t *testing.T) {
	ctx := context.Background()
	store := &memoryStore{document: Document{Version: 1}}
	var order []string
	store.onSave = func() { order = append(order, "save") }
	service, err := NewService(store, func(_ context.Context, text string) (string, error) {
		if !strings.Contains(text, "[SCHEDULE REMINDER]") || !strings.Contains(text, `reminder_prompt_json: "Check it"`) {
			t.Fatalf("framing = %q", text)
		}
		order = append(order, "deliver")
		return "message-1", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return start }
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	_, err = service.Create(ctx, func(now time.Time) (Record, error) { return NewAfter("a", "Check", "Check it", 60, now) })
	if err != nil {
		t.Fatal(err)
	}
	order = nil
	service.mu.Lock()
	service.now = func() time.Time { return start.Add(2 * time.Minute) }
	service.mu.Unlock()
	if failed := service.dispatchDue(ctx, nil); len(failed) != 0 {
		t.Fatalf("dispatch failures: %#v", failed)
	}
	if len(order) != 2 || order[0] != "deliver" || order[1] != "save" {
		t.Fatalf("persist order = %#v", order)
	}
	if listed, err := service.List(); err != nil || len(listed) != 0 {
		t.Fatalf("active list = %#v, %v", listed, err)
	}
	document, err := store.Load(ctx)
	if err != nil || len(document.Tasks) != 1 || document.Tasks[0].Status != "ended" ||
		document.Tasks[0].LastDelivery == nil || document.Tasks[0].LastDelivery.MessageID != "message-1" {
		t.Fatalf("saved task = %+v, %v", document, err)
	}
}

func TestServiceBatchesRecurringAndKeepsLatestMissedOccurrence(t *testing.T) {
	ctx := context.Background()
	store := &memoryStore{document: Document{Version: 1}}
	var messages []string
	service, err := NewService(store, func(_ context.Context, text string) (string, error) {
		messages = append(messages, text)
		return "batch-message", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return start }
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	for _, id := range []string{"a", "b"} {
		_, err := service.Create(ctx, func(now time.Time) (Record, error) { return NewEvery(id, "Check", id, 60, now) })
		if err != nil {
			t.Fatal(err)
		}
	}
	service.mu.Lock()
	service.now = func() time.Time { return start.Add(5*time.Minute + 10*time.Second) }
	service.mu.Unlock()
	if failed := service.dispatchDue(ctx, nil); len(failed) != 0 {
		t.Fatalf("dispatch failures: %#v", failed)
	}
	if len(messages) != 1 || !strings.Contains(messages[0], "[SCHEDULE REMINDER BATCH]") ||
		!strings.Contains(messages[0], `"occurrence_at":"2099-01-01T00:05:00.000Z"`) {
		t.Fatalf("batch = %#v", messages)
	}
	listed, err := service.List()
	if err != nil || len(listed) != 2 {
		t.Fatalf("list = %#v, %v", listed, err)
	}
	for _, task := range listed {
		if task.Record.ScheduledAt != "2099-01-01T00:06:00.000Z" || task.LastDelivery.MessageID != "batch-message" {
			t.Fatalf("advanced task = %+v", task)
		}
	}
}

func TestServicePersistsDeliveryGapAsAtLeastOnce(t *testing.T) {
	ctx := context.Background()
	store := &memoryStore{document: Document{Version: 1}}
	count := 0
	service, err := NewService(store, func(context.Context, string) (string, error) {
		count++
		return "delivered", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return start }
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.Create(ctx, func(now time.Time) (Record, error) { return NewAfter("a", "Check", "Again", 60, now) }); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	service.now = func() time.Time { return start.Add(2 * time.Minute) }
	service.mu.Unlock()
	store.mu.Lock()
	store.failSave = true
	store.mu.Unlock()
	if failed := service.dispatchDue(ctx, nil); !failed["a"] {
		t.Fatalf("expected task-write failure: %#v", failed)
	}
	if _, err := service.List(); err == nil {
		t.Fatal("uncertain task write left the service readable")
	}
	service.Close()
	store.mu.Lock()
	store.failSave = false
	store.mu.Unlock()
	recovered, err := NewService(store, func(context.Context, string) (string, error) {
		count++
		return "delivered-again", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	recovered.now = func() time.Time { return start.Add(2 * time.Minute) }
	if err := recovered.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if failed := recovered.dispatchDue(ctx, nil); len(failed) != 0 {
		t.Fatalf("retry failed: %#v", failed)
	}
	if count != 2 {
		t.Fatalf("delivery count = %d, want at-least-once duplicate", count)
	}
}
