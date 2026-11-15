package schedule

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func noOpHostPrepare(context.Context, string) error { return nil }

func TestHostScheduleStopsDispatchingAfterCancellationDuringDelivery(t *testing.T) {
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	first, err := NewAfter("first", "First", "First reminder", 60, start)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAfter("second", "Second", "Second reminder", 60, start)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{document: Document{Version: 1, Tasks: []Task{
		{SessionID: "owner", Record: first, Status: "active", DeliveryHistory: History{Records: []Receipt{}}},
		{SessionID: "owner", Record: second, Status: "active", DeliveryHistory: History{Records: []Receipt{}}},
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delivered []string
	service, err := NewHostService(store, noOpHostPrepare, func(_ context.Context, _ string, text string) (string, error) {
		if strings.Contains(text, `schedule_id_json: "first"`) {
			delivered = append(delivered, "first")
			cancel()
			return "first-message", nil
		}
		delivered = append(delivered, "second")
		return "second-message", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	service.now = func() time.Time { return start.Add(2 * time.Minute) }
	service.mu.Unlock()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.dispatchDue(ctx, nil)
	if len(delivered) != 1 || delivered[0] != "first" {
		t.Fatalf("delivery continued after cancellation: %v", delivered)
	}
}

func TestHostScheduleStopsBeforeDeliveryWhenPreparationIsCancelled(t *testing.T) {
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	record, err := NewAfter("first", "First", "First reminder", 60, start)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{document: Document{Version: 1, Tasks: []Task{{
		SessionID: "owner", Record: record, Status: "active", DeliveryHistory: History{Records: []Receipt{}},
	}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delivered bool
	service, err := NewHostService(store, func(context.Context, string) error {
		cancel()
		return nil
	}, func(context.Context, string, string) (string, error) {
		delivered = true
		return "message", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	service.now = func() time.Time { return start.Add(2 * time.Minute) }
	service.mu.Unlock()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.dispatchDue(ctx, nil)
	if delivered {
		t.Fatal("Schedule delivered after preparation was cancelled")
	}
}

func TestHostScheduleListKeepsInsertionOrderAcrossUpdatesAndReopen(t *testing.T) {
	store := FileStore{Path: filepath.Join(t.TempDir(), "schedule.json")}
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	open := func() *Service {
		service, err := NewHostService(store, noOpHostPrepare,
			func(context.Context, string, string) (string, error) { return "", errors.New("unexpected delivery") })
		if err != nil {
			t.Fatal(err)
		}
		service.now = func() time.Time { return start }
		if err := service.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		return service
	}
	service := open()
	first, err := service.CreateFor(context.Background(), "first", func(now time.Time) (Record, error) {
		return NewAfter("z-first", "First", "Later target", 3600, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateFor(context.Background(), "first", func(now time.Time) (Record, error) {
		return NewAfter("a-second", "Second", "Earlier target", 60, now)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateFor(context.Background(), "first", "z-first", first.Record, func(record Record, _ time.Time) (Record, error) {
		record.Title = "Updated first"
		return record, nil
	}); err != nil {
		t.Fatal(err)
	}
	service.Close()

	service = open()
	listed, err := service.ListFor("first")
	if err != nil || len(listed) != 2 || listed[0].Record.ID != "z-first" || listed[1].Record.ID != "a-second" || listed[0].Record.Title != "Updated first" {
		t.Fatalf("restored insertion order = %+v, %v", listed, err)
	}
	if deleted, err := service.DeleteFor(context.Background(), "first", "z-first"); err != nil || !deleted {
		t.Fatalf("delete first task = %t, %v", deleted, err)
	}
	if _, err := service.CreateFor(context.Background(), "first", func(now time.Time) (Record, error) {
		return NewAfter("z-first", "Recreated", "New task", 3600, now)
	}); err != nil {
		t.Fatal(err)
	}
	service.Close()

	service = open()
	defer service.Close()
	listed, err = service.ListFor("first")
	if err != nil || len(listed) != 2 || listed[0].Record.ID != "a-second" || listed[1].Record.ID != "z-first" || listed[1].Record.Title != "Recreated" {
		t.Fatalf("recreated insertion order = %+v, %v", listed, err)
	}
}

func TestHostScheduleDueOrderKeepsFirstRecurringBatchPosition(t *testing.T) {
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	first, err := NewEvery("z-recurring", "First", "First recurring", 60, start)
	if err != nil {
		t.Fatal(err)
	}
	middle, err := NewAfter("middle-one-shot", "Middle", "One shot", 60, start)
	if err != nil {
		t.Fatal(err)
	}
	last, err := NewEvery("a-recurring", "Last", "Last recurring", 60, start)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{document: Document{Version: 1, Tasks: []Task{
		{SessionID: "first", Record: first, Status: "active", DeliveryHistory: History{Records: []Receipt{}}},
		{SessionID: "first", Record: middle, Status: "active", DeliveryHistory: History{Records: []Receipt{}}},
		{SessionID: "first", Record: last, Status: "active", DeliveryHistory: History{Records: []Receipt{}}},
	}}}
	var clock atomic.Int64
	clock.Store(start.UnixMilli())
	var deliveries []string
	service, err := NewHostService(store, noOpHostPrepare, func(_ context.Context, _ string, text string) (string, error) {
		deliveries = append(deliveries, text)
		return fmt.Sprintf("message-%d", len(deliveries)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.UnixMilli(clock.Load()) }
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	clock.Store(start.Add(2 * time.Minute).UnixMilli())
	if failed := service.dispatchDue(context.Background(), nil); len(failed) != 0 {
		t.Fatalf("ordered Schedule delivery failed: %+v", failed)
	}
	if len(deliveries) != 2 || !strings.Contains(deliveries[0], "[SCHEDULE REMINDER BATCH]") ||
		!strings.Contains(deliveries[0], `"schedule_id":"z-recurring"`) ||
		!strings.Contains(deliveries[0], `"schedule_id":"a-recurring"`) ||
		strings.Contains(deliveries[0], "middle-one-shot") || !strings.Contains(deliveries[1], "middle-one-shot") {
		t.Fatalf("delivery order or recurring batch differs from upstream: %q", deliveries)
	}
}

func TestHostScheduleCreateAnchorsRuleBeforeSerializedWrite(t *testing.T) {
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixMilli())
	service, err := NewHostService(&memoryStore{document: Document{Version: 1}}, noOpHostPrepare,
		func(context.Context, string, string) (string, error) { return "", errors.New("unexpected delivery") })
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.UnixMilli(clock.Load()) }
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	// Simulate an earlier Schedule operation holding the serialized queue.
	// Rule construction must happen before this admission wait, as upstream.
	service.mu.Lock()
	built := make(chan struct{})
	type createResult struct {
		task Task
		err  error
	}
	finished := make(chan createResult, 1)
	go func() {
		task, err := service.CreateFor(context.Background(), "first", func(now time.Time) (Record, error) {
			close(built)
			return NewAfter("queued", "Review", "Check the draft", 60, now)
		})
		finished <- createResult{task: task, err: err}
	}()
	select {
	case <-built:
	case <-time.After(time.Second):
		service.mu.Unlock()
		<-finished
		t.Fatal("Schedule rule was constructed only after joining the serialized queue")
	}
	clock.Store(start.Add(5 * time.Minute).UnixMilli())
	service.mu.Unlock()
	result := <-finished
	if result.err != nil {
		t.Fatal(result.err)
	}
	want := start.Add(time.Minute).Format("2006-01-02T15:04:05.000Z")
	if result.task.Record.ScheduledAt != want {
		t.Fatalf("queued create target = %s, want request-time target %s", result.task.Record.ScheduledAt, want)
	}
}

type scheduleAcknowledgementLossStore struct {
	Store
	failAfter int
	saves     int
}

func (s *scheduleAcknowledgementLossStore) Save(ctx context.Context, document Document) error {
	s.saves++
	if err := s.Store.Save(ctx, document); err != nil {
		return err
	}
	if s.saves == s.failAfter {
		return errors.New("Schedule write acknowledgment lost")
	}
	return nil
}

func TestHostScheduleCancellationBeforePersistenceKeepsServiceUsable(t *testing.T) {
	store := &memoryStore{document: Document{Version: 1}}
	service, err := NewHostService(store, noOpHostPrepare, func(context.Context, string, string) (string, error) {
		return "", errors.New("unexpected delivery")
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	createCtx, cancelCreate := context.WithCancel(context.Background())
	_, err = service.CreateFor(createCtx, "first", func(now time.Time) (Record, error) {
		record, err := NewAfter("cancelled", "Check", "No delivery", 3600, now)
		cancelCreate()
		return record, err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled create = %v", err)
	}
	if tasks, err := service.ListFor("first"); err != nil || len(tasks) != 0 {
		t.Fatalf("cancelled create changed or poisoned Schedule: %+v, %v", tasks, err)
	}

	created, err := service.CreateFor(context.Background(), "first", func(now time.Time) (Record, error) {
		return NewAfter("review", "Review", "Check the draft", 3600, now)
	})
	if err != nil {
		t.Fatalf("create after cancellation: %v", err)
	}
	updateCtx, cancelUpdate := context.WithCancel(context.Background())
	_, err = service.UpdateFor(updateCtx, "first", "review", created.Record, func(record Record, _ time.Time) (Record, error) {
		record.Title = "Uncommitted edit"
		cancelUpdate()
		return record, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled update = %v", err)
	}
	current, found, err := service.LookupFor("first", "review")
	if err != nil || !found || current.Record.Title != "Review" {
		t.Fatalf("cancelled update changed or poisoned Schedule: %+v, found=%t, err=%v", current, found, err)
	}
	updated, err := service.UpdateFor(context.Background(), "first", "review", current.Record, func(record Record, _ time.Time) (Record, error) {
		record.Title = "Committed edit"
		return record, nil
	})
	if err != nil || updated.Record.Title != "Committed edit" {
		t.Fatalf("update after cancellation = %+v, %v", updated, err)
	}
}

func TestHostSchedulePartialBatchReceiptFailureRecoversOnlyPendingTask(t *testing.T) {
	ctx := context.Background()
	store := &memoryStore{document: Document{Version: 1}}
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	var batches []string
	newService := func(now time.Time) *Service {
		service, err := NewHostService(store, noOpHostPrepare, func(_ context.Context, sessionID, text string) (string, error) {
			if sessionID != "first" {
				t.Fatalf("reminder crossed Session boundary: %q", sessionID)
			}
			batches = append(batches, text)
			return fmt.Sprintf("batch-%d", len(batches)), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		service.now = func() time.Time { return now }
		if err := service.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return service
	}
	service := newService(start)
	for _, id := range []string{"a", "b"} {
		if _, err := service.CreateFor(ctx, "first", func(time.Time) (Record, error) {
			return NewEvery(id, "Review", id, 60, start)
		}); err != nil {
			t.Fatal(err)
		}
	}
	service.mu.Lock()
	service.now = func() time.Time { return start.Add(2 * time.Minute) }
	service.mu.Unlock()
	// The first receipt lands. The second write fails after the shared inbox
	// message was delivered; its task row still points at the overdue target.
	store.onSave = func() { store.failSave = true }
	failed := service.dispatchDue(ctx, nil)
	if !failed["b"] || failed["a"] || len(batches) != 1 ||
		!strings.Contains(batches[0], `"schedule_id":"a"`) || !strings.Contains(batches[0], `"schedule_id":"b"`) {
		t.Fatalf("partial batch failure = %+v, deliveries=%q", failed, batches)
	}
	if _, err := service.ListFor("first"); err == nil {
		t.Fatal("uncertain second receipt left the live Schedule readable")
	}
	service.Close()
	store.failSave, store.onSave = false, nil

	recovered := newService(start.Add(2 * time.Minute))
	defer recovered.Close()
	if failed := recovered.dispatchDue(ctx, nil); len(failed) != 0 {
		t.Fatalf("recovered batch failed: %+v", failed)
	}
	if len(batches) != 2 || strings.Contains(batches[1], `"schedule_id":"a"`) ||
		!strings.Contains(batches[1], `"schedule_id":"b"`) {
		t.Fatalf("recovery redelivered a committed member or lost the pending one: %q", batches)
	}
	first, found, err := recovered.LookupFor("first", "a")
	if err != nil || !found || first.LastDelivery == nil || first.LastDelivery.MessageID != "batch-1" ||
		len(first.DeliveryHistory.Records) != 1 {
		t.Fatalf("committed first receipt changed on recovery: %+v, found=%t, err=%v", first, found, err)
	}
	second, found, err := recovered.LookupFor("first", "b")
	if err != nil || !found || second.LastDelivery == nil || second.LastDelivery.MessageID != "batch-2" ||
		len(second.DeliveryHistory.Records) != 1 {
		t.Fatalf("pending second receipt was not committed on retry: %+v, found=%t, err=%v", second, found, err)
	}
}

func TestHostScheduleCommittedReceiptWithLostAcknowledgementDoesNotRedeliver(t *testing.T) {
	ctx := context.Background()
	base := FileStore{Path: filepath.Join(t.TempDir(), "schedule.json")}
	store := &scheduleAcknowledgementLossStore{Store: base, failAfter: 4}
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	deliveries := 0
	newService := func(backing Store, now time.Time) *Service {
		service, err := NewHostService(backing, noOpHostPrepare, func(_ context.Context, _ string, text string) (string, error) {
			deliveries++
			if !strings.Contains(text, `"schedule_id":"a"`) || !strings.Contains(text, `"schedule_id":"b"`) {
				t.Fatalf("recurring batch lost a member: %q", text)
			}
			return "batch-1", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		service.now = func() time.Time { return now }
		if err := service.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return service
	}
	service := newService(store, start)
	for _, id := range []string{"a", "b"} {
		if _, err := service.CreateFor(ctx, "first", func(time.Time) (Record, error) {
			return NewEvery(id, "Review", id, 60, start)
		}); err != nil {
			t.Fatal(err)
		}
	}
	service.mu.Lock()
	service.now = func() time.Time { return start.Add(2 * time.Minute) }
	service.mu.Unlock()
	if failed := service.dispatchDue(ctx, nil); !failed["b"] || failed["a"] || deliveries != 1 {
		t.Fatalf("lost second receipt acknowledgment = %+v, deliveries=%d", failed, deliveries)
	}
	if _, err := service.ListFor("first"); err == nil {
		t.Fatal("uncertain receipt write left the live Schedule readable")
	}
	service.Close()

	// The second row was durably committed despite the reported error. Reopen
	// the file, not the poisoned in-memory projection; neither task is due.
	recovered := newService(base, start.Add(2*time.Minute))
	defer recovered.Close()
	if failed := recovered.dispatchDue(ctx, nil); len(failed) != 0 || deliveries != 1 {
		t.Fatalf("recovery redelivered a committed batch: failed=%+v deliveries=%d", failed, deliveries)
	}
	for _, id := range []string{"a", "b"} {
		task, found, err := recovered.LookupFor("first", id)
		if err != nil || !found || task.LastDelivery == nil || task.LastDelivery.MessageID != "batch-1" ||
			len(task.DeliveryHistory.Records) != 1 {
			t.Fatalf("task %s did not restore its committed receipt: %+v, found=%t, err=%v", id, task, found, err)
		}
	}
}

func TestHostScheduleScopesManagementAndBatchesPerSession(t *testing.T) {
	ctx := context.Background()
	store := &memoryStore{document: Document{Version: 1}}
	type delivery struct{ sessionID, text string }
	var deliveries []delivery
	service, err := NewHostService(store, noOpHostPrepare, func(_ context.Context, sessionID, text string) (string, error) {
		deliveries = append(deliveries, delivery{sessionID, text})
		return "message-" + sessionID, nil
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
	for _, item := range []struct{ sessionID, id string }{
		{"first", "a"}, {"first", "b"}, {"second", "c"},
	} {
		item := item
		if _, err := service.CreateFor(ctx, item.sessionID, func(now time.Time) (Record, error) {
			return NewEvery(item.id, "Check", item.id, 60, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if !service.HasActive() {
		t.Fatal("Host lost its active timer obligation")
	}
	first, err := service.ListFor("first")
	if err != nil || len(first) != 2 {
		t.Fatalf("first Session tasks = %+v, %v", first, err)
	}
	second, err := service.ListFor("second")
	if err != nil || len(second) != 1 {
		t.Fatalf("second Session tasks = %+v, %v", second, err)
	}
	if _, found, err := service.LookupFor("second", "a"); err != nil || found {
		t.Fatalf("foreign lookup = %t, %v", found, err)
	}
	if deleted, err := service.DeleteFor(ctx, "second", "a"); err != nil || deleted {
		t.Fatalf("foreign delete = %t, %v", deleted, err)
	}
	service.mu.Lock()
	service.now = func() time.Time { return start.Add(5*time.Minute + 10*time.Second) }
	service.mu.Unlock()
	if failed := service.dispatchDue(ctx, nil); len(failed) != 0 {
		t.Fatalf("Host deliveries failed: %+v", failed)
	}
	if len(deliveries) != 2 {
		t.Fatalf("recurring reminders crossed Session boundary: %+v", deliveries)
	}
	for _, item := range deliveries {
		if !strings.Contains(item.text, "[SCHEDULE REMINDER BATCH]") {
			t.Fatalf("non-batch delivery: %+v", item)
		}
		if item.sessionID == "first" && (!strings.Contains(item.text, `"schedule_id":"a"`) || !strings.Contains(item.text, `"schedule_id":"b"`) || strings.Contains(item.text, `"schedule_id":"c"`)) {
			t.Fatalf("first Session batch crossed boundary: %s", item.text)
		}
		if item.sessionID == "second" && (!strings.Contains(item.text, `"schedule_id":"c"`) || strings.Contains(item.text, `"schedule_id":"a"`)) {
			t.Fatalf("second Session batch crossed boundary: %s", item.text)
		}
	}
	document, err := store.Load(ctx)
	if err != nil || len(document.Tasks) != 3 {
		t.Fatalf("Host task document = %+v, %v", document, err)
	}
	for _, task := range document.Tasks {
		if task.SessionID == "" || task.LastDelivery == nil || task.LastDelivery.MessageID != "message-"+task.SessionID {
			t.Fatalf("task lost Session binding or receipt: %+v", task)
		}
	}
}

func TestHostScheduleUpdateKeepsOwnerAndHistoryAndRejectsStaleRecord(t *testing.T) {
	ctx := context.Background()
	store := &memoryStore{document: Document{Version: 1}}
	service, err := NewHostService(store, noOpHostPrepare, func(context.Context, string, string) (string, error) {
		return "delivered-message", nil
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
	created, err := service.CreateFor(ctx, "first", func(now time.Time) (Record, error) {
		return NewEvery("review", "Review", "Check the draft", 60, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	service.now = func() time.Time { return start.Add(2 * time.Minute) }
	service.mu.Unlock()
	if failed := service.dispatchDue(ctx, nil); len(failed) != 0 {
		t.Fatalf("initial delivery failed: %+v", failed)
	}
	current, found, err := service.LookupFor("first", "review")
	if err != nil || !found || current.LastDelivery == nil || len(current.DeliveryHistory.Records) != 1 {
		t.Fatalf("delivered reminder = %+v, found=%t, err=%v", current, found, err)
	}
	if _, err := service.UpdateFor(ctx, "second", "review", current.Record, func(record Record, _ time.Time) (Record, error) {
		record.Title = "Foreign edit"
		return record, nil
	}); err == nil || err.Error() != "schedule_not_found" {
		t.Fatalf("foreign Session update = %v", err)
	}
	updated, err := service.UpdateFor(ctx, "first", "review", current.Record, func(record Record, _ time.Time) (Record, error) {
		record.Title, record.Prompt = "Review again", "Check the revised draft"
		return record, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.SessionID != "first" || updated.Record.ScheduledAt != current.Record.ScheduledAt ||
		updated.LastDelivery == nil || updated.LastDelivery.MessageID != current.LastDelivery.MessageID ||
		len(updated.DeliveryHistory.Records) != 1 || updated.DeliveryHistory.Records[0].MessageID != current.DeliveryHistory.Records[0].MessageID {
		t.Fatalf("in-place update lost owner, target, or history: %+v", updated)
	}
	if _, err := service.UpdateFor(ctx, "first", "review", current.Record, func(record Record, _ time.Time) (Record, error) {
		record.Title = "Stale edit"
		return record, nil
	}); err == nil || err.Error() != "schedule_conflict" {
		t.Fatalf("stale update = %v", err)
	}
	if _, err := service.UpdateFor(ctx, "first", "review", updated.Record, func(record Record, now time.Time) (Record, error) {
		return NewEvery(record.ID, record.Title, record.Prompt, 120, now)
	}); err != nil {
		t.Fatal(err)
	}
	service.Close()
	restored, err := NewHostService(store, noOpHostPrepare, func(context.Context, string, string) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	after, found, err := restored.LookupFor("first", "review")
	if err != nil || !found || after.SessionID != "first" || after.Record.ID != created.Record.ID ||
		after.Record.ScheduledAt != "2099-01-01T00:04:00.000Z" || after.Record.Title != "Review again" ||
		after.LastDelivery == nil || after.LastDelivery.MessageID != "delivered-message" || len(after.DeliveryHistory.Records) != 1 {
		t.Fatalf("updated reminder did not recover intact: %+v, found=%t, err=%v", after, found, err)
	}
}

func TestHostScheduleRejectsUnboundRestoredTask(t *testing.T) {
	store := &memoryStore{document: Document{Version: 1, Tasks: []Task{{Record: Record{ID: "legacy"}, Status: "active"}}}}
	service, err := NewHostService(store, noOpHostPrepare, func(context.Context, string, string) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err == nil {
		t.Fatal("Host Schedule accepted a task with no owner Session")
	}
}

func TestHostScheduleMixedOneShotAndRecurringDueTasks(t *testing.T) {
	ctx := context.Background()
	store := &memoryStore{document: Document{Version: 1}}
	type delivery struct{ sessionID, text string }
	var deliveries []delivery
	service, err := NewHostService(store, noOpHostPrepare, func(_ context.Context, sessionID, text string) (string, error) {
		deliveries = append(deliveries, delivery{sessionID, text})
		return "message-" + string(rune('a'+len(deliveries)-1)), nil
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
	for _, item := range []struct {
		sessionID, id, kind string
	}{
		{"first", "once-first", "after"},
		{"first", "repeat-first", "every"},
		{"second", "once-second", "after"},
	} {
		item := item
		if _, err := service.CreateFor(ctx, item.sessionID, func(now time.Time) (Record, error) {
			if item.kind == "every" {
				return NewEvery(item.id, "Check", item.id, 60, now)
			}
			return NewAfter(item.id, "Check", item.id, 60, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	service.mu.Lock()
	service.now = func() time.Time { return start.Add(5*time.Minute + 10*time.Second) }
	service.mu.Unlock()
	if failed := service.dispatchDue(ctx, nil); len(failed) != 0 {
		t.Fatalf("mixed dispatch failures: %+v", failed)
	}
	if len(deliveries) != 3 {
		t.Fatalf("mixed due tasks produced %d messages, want two one-shot and one recurring: %+v", len(deliveries), deliveries)
	}
	for _, item := range deliveries {
		switch item.sessionID {
		case "first":
			if strings.Contains(item.text, "once-second") {
				t.Fatalf("first Session received second Session reminder: %s", item.text)
			}
		case "second":
			if strings.Contains(item.text, "once-first") || strings.Contains(item.text, "repeat-first") {
				t.Fatalf("second Session received first Session reminder: %s", item.text)
			}
		default:
			t.Fatalf("unknown Session delivery: %+v", item)
		}
	}
	document, err := store.Load(ctx)
	if err != nil || len(document.Tasks) != 3 {
		t.Fatalf("mixed task document = %+v, %v", document, err)
	}
	for _, task := range document.Tasks {
		if task.LastDelivery == nil {
			t.Fatalf("mixed task lost receipt: %+v", task)
		}
		if task.Record.Kind == "every" {
			if task.Status != "active" || task.Record.ScheduledAt != "2099-01-01T00:06:00.000Z" {
				t.Fatalf("recurring task not advanced: %+v", task)
			}
		} else if task.Status != "ended" {
			t.Fatalf("one-shot task remained active: %+v", task)
		}
	}
}

func TestHostScheduleExhaustedRecurrenceRetainsLastOccurrence(t *testing.T) {
	ctx := context.Background()
	store := &memoryStore{document: Document{Version: 1}}
	service, err := NewHostService(store, noOpHostPrepare, func(context.Context, string, string) (string, error) { return "last-message", nil })
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(9999, 12, 31, 23, 55, 0, 0, time.UTC)
	service.now = func() time.Time { return start }
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.CreateFor(ctx, "first", func(now time.Time) (Record, error) {
		return NewEvery("ending", "End", "Last delivery", 60, now)
	}); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	service.now = func() time.Time { return start.Add(4*time.Minute + 30*time.Second) }
	service.mu.Unlock()
	if failed := service.dispatchDue(ctx, nil); len(failed) != 0 {
		t.Fatalf("terminal recurring dispatch failed: %+v", failed)
	}
	document, err := store.Load(ctx)
	if err != nil || len(document.Tasks) != 1 {
		t.Fatalf("terminal recurring document = %+v, %v", document, err)
	}
	task := document.Tasks[0]
	if task.Status != "ended" || task.Record.ScheduledAt != "9999-12-31T23:59:00.000Z" ||
		task.LastDelivery == nil || task.LastDelivery.ScheduledAt != task.Record.ScheduledAt {
		t.Fatalf("terminal recurring task did not retain the final occurrence: %+v", task)
	}
}

func TestHostScheduleResamplesAfterColdSessionPreparation(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixMilli())
	var delivered string
	service, err := NewHostService(&memoryStore{document: Document{Version: 1}},
		func(context.Context, string) error {
			clock.Store(start.Add(5*time.Minute + 30*time.Second).UnixMilli())
			return nil
		},
		func(_ context.Context, _ string, text string) (string, error) {
			delivered = text
			return "latest-message", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.UnixMilli(clock.Load()) }
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.CreateFor(ctx, "original", func(now time.Time) (Record, error) {
		return NewEvery("recurring", "Check", "Follow up", 60, now)
	}); err != nil {
		t.Fatal(err)
	}
	clock.Store(start.Add(time.Minute).UnixMilli())
	if failed := service.dispatchDue(ctx, nil); len(failed) != 0 {
		t.Fatalf("dispatch failed: %+v", failed)
	}
	if !strings.Contains(delivered, `"occurrence_at":"2099-01-01T00:05:00.000Z"`) {
		t.Fatalf("reminder used pre-restore time: %s", delivered)
	}
	task, found, err := service.LookupFor("original", "recurring")
	if err != nil || !found || task.Record.ScheduledAt != "2099-01-01T00:06:00.000Z" ||
		task.LastDelivery == nil || task.LastDelivery.ScheduledAt != "2099-01-01T00:05:00.000Z" {
		t.Fatalf("post-restore occurrence was not committed: %+v, found=%t, err=%v", task, found, err)
	}
}

func TestHostScheduleRollbackDuringPreparationKeepsFutureObligations(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixMilli())
	var deliveries atomic.Int64
	service, err := NewHostService(&memoryStore{document: Document{Version: 1}},
		func(context.Context, string) error {
			clock.Store(start.Add(30 * time.Second).UnixMilli())
			return nil
		},
		func(context.Context, string, string) (string, error) {
			deliveries.Add(1)
			return "unexpected-message", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.UnixMilli(clock.Load()) }
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	for _, build := range []RuleBuilder{
		func(now time.Time) (Record, error) { return NewAfter("once", "Once", "Check", 60, now) },
		func(now time.Time) (Record, error) { return NewEvery("repeat", "Repeat", "Check", 60, now) },
	} {
		if _, err := service.CreateFor(ctx, "original", build); err != nil {
			t.Fatal(err)
		}
	}
	clock.Store(start.Add(time.Minute).UnixMilli())
	if failed := service.dispatchDue(ctx, nil); len(failed) != 0 {
		t.Fatalf("rolled-back tasks were marked failed: %+v", failed)
	}
	if deliveries.Load() != 0 {
		t.Fatalf("rolled-back tasks delivered %d messages", deliveries.Load())
	}
	for _, id := range []string{"once", "repeat"} {
		task, found, err := service.LookupFor("original", id)
		if err != nil || !found || task.Status != "active" || task.Record.ScheduledAt != "2099-01-01T00:01:00.000Z" || task.LastDelivery != nil {
			t.Fatalf("rolled-back task %s lost its future target: %+v, found=%t, err=%v", id, task, found, err)
		}
	}
}

func TestHostScheduleRollbackAdmitsOnlyStillDueBatchMembers(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixMilli())
	var delivered string
	service, err := NewHostService(&memoryStore{document: Document{Version: 1}},
		func(context.Context, string) error {
			clock.Store(start.Add(time.Minute + 30*time.Second).UnixMilli())
			return nil
		},
		func(_ context.Context, _ string, text string) (string, error) {
			delivered = text
			return "batch-message", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.UnixMilli(clock.Load()) }
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.CreateFor(ctx, "original", func(now time.Time) (Record, error) {
		return NewEvery("first", "First", "First prompt", 60, now)
	}); err != nil {
		t.Fatal(err)
	}
	clock.Store(start.Add(time.Minute).UnixMilli())
	if _, err := service.CreateFor(ctx, "original", func(now time.Time) (Record, error) {
		return NewEvery("second", "Second", "Second prompt", 60, now)
	}); err != nil {
		t.Fatal(err)
	}
	clock.Store(start.Add(2 * time.Minute).UnixMilli())
	if failed := service.dispatchDue(ctx, nil); len(failed) != 0 {
		t.Fatalf("partial rollback dispatch failed: %+v", failed)
	}
	if !strings.Contains(delivered, `"schedule_id":"first"`) || strings.Contains(delivered, `"schedule_id":"second"`) {
		t.Fatalf("rolled-back batch included a future member: %s", delivered)
	}
	for _, id := range []string{"first", "second"} {
		task, found, err := service.LookupFor("original", id)
		if err != nil || !found || task.Record.ScheduledAt != "2099-01-01T00:02:00.000Z" {
			t.Fatalf("task %s lost next target: %+v, found=%t, err=%v", id, task, found, err)
		}
		if id == "first" && (task.LastDelivery == nil || task.LastDelivery.ScheduledAt != "2099-01-01T00:01:00.000Z") {
			t.Fatalf("due member lacked receipt: %+v", task)
		}
		if id == "second" && task.LastDelivery != nil {
			t.Fatalf("future member acquired a receipt: %+v", task)
		}
	}
}

func TestHostScheduleAppendPreservesLegacyLastDelivery(t *testing.T) {
	ctx := context.Background()
	legacy := Receipt{
		ScheduledAt: "2098-12-31T23:59:00.000Z",
		DeliveredAt: "2099-01-01T00:00:00.000Z",
		MessageID:   "legacy-message",
	}
	record := Record{
		ID: "repeat", Kind: "every", Title: "Repeat", Prompt: "Check",
		EverySeconds: 60, ScheduledAt: "2099-01-01T00:01:00.000Z",
	}
	store := &memoryStore{document: Document{Version: 1, Tasks: []Task{{
		SessionID: "original", Record: record, Status: "active", LastDelivery: &legacy,
	}}}}
	service, err := NewHostService(store, noOpHostPrepare, func(context.Context, string, string) (string, error) {
		return "new-message", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2099, 1, 1, 0, 1, 30, 0, time.UTC)
	service.now = func() time.Time { return now }
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if failed := service.dispatchDue(ctx, nil); len(failed) != 0 {
		t.Fatalf("legacy task delivery failed: %+v", failed)
	}
	task, found, err := service.LookupFor("original", "repeat")
	if err != nil || !found || len(task.DeliveryHistory.Records) != 2 ||
		task.DeliveryHistory.Records[0].MessageID != "legacy-message" ||
		task.DeliveryHistory.Records[1].MessageID != "new-message" ||
		!task.DeliveryHistory.EarlierRecordsUnavailable || task.DeliveryHistory.EarlierRecordsPruned {
		t.Fatalf("legacy receipt was not preserved in appended history: %+v, found=%t, err=%v", task, found, err)
	}
}

func TestHostScheduleNewTaskHasKnownEmptyHistory(t *testing.T) {
	ctx := context.Background()
	service, err := NewHostService(&memoryStore{document: Document{Version: 1}}, noOpHostPrepare,
		func(context.Context, string, string) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.CreateFor(ctx, "original", func(now time.Time) (Record, error) {
		return NewAfter("new", "New", "Check", 60, now)
	}); err != nil {
		t.Fatal(err)
	}
	page, err := service.HistoryFor("original", "new", 10, nil)
	if err != nil || len(page.Records) != 0 || page.EarlierRecordsUnavailable || page.EarlierRecordsPruned {
		t.Fatalf("new task history is not known empty: %+v, %v", page, err)
	}
}

func TestSchedulePerDeliveryReservationSurvivesFailedCreationAndReopen(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.Unix())
	store := FileStore{Path: filepath.Join(t.TempDir(), "schedule.json")}
	var reserved, delivered atomic.Value
	reserved.Store("")
	delivered.Store("")
	open := func(create HostPrepare) *Service {
		t.Helper()
		service, err := NewHostService(store, noOpHostPrepare, func(_ context.Context, target, _ string) (string, error) {
			delivered.Store(target)
			return "message", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		service.now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
		service.SetSessionCreator(create)
		if err := service.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return service
	}
	service := open(func(_ context.Context, id string) error {
		reserved.Store(id)
		return errors.New("creation temporarily unavailable")
	})
	t.Cleanup(service.Close)
	_, err := service.CreateForTarget(ctx, "owner", &Target{Kind: "new"}, func(at time.Time) (Record, error) {
		return NewAfter("task", "Task", "Check", 60, at)
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Store(start.Add(time.Minute).Unix())
	failed := service.dispatchDue(ctx, nil)["task"]
	service.Close()
	reservation := reserved.Load().(string)
	if !failed || reservation == "" || delivered.Load().(string) != "" {
		t.Fatalf("failed first dispatch: reserved=%q delivered=%q", reservation, delivered.Load())
	}

	service = open(func(_ context.Context, id string) error {
		if id != reservation {
			return fmt.Errorf("used %q instead of the persisted reservation %q", id, reservation)
		}
		return nil
	})
	defer service.Close()
	if failures := service.dispatchDue(ctx, nil); len(failures) != 0 {
		t.Fatalf("delivery failed after reopening: %v", failures)
	}
	task, found, err := service.LookupFor("owner", "task")
	if err != nil || !found || task.PendingDelivery != nil || task.LastDelivery == nil ||
		task.LastDelivery.TargetSessionID != reservation || delivered.Load().(string) != reservation || task.Status != "ended" {
		t.Fatalf("restored task: %+v, %t, %v", task, found, err)
	}
}
