package hostruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/assembly"
	"go-bot/harness/schedule"
	"go-bot/harness/session"
	"go-bot/internal/testsupport/session"
)

type workerScheduleStore struct {
	mu       sync.Mutex
	document schedule.Document
}

func (s *workerScheduleStore) Load(context.Context) (schedule.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(s.document)
	var result schedule.Document
	_ = json.Unmarshal(data, &result)
	return result, nil
}

func (s *workerScheduleStore) Save(_ context.Context, document schedule.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(document)
	return json.Unmarshal(data, &s.document)
}

type scheduledTurnModel struct{}

func (scheduledTurnModel) Complete(_ context.Context, messages []agent.Message, specs []agent.ToolSpec) (session.AssistantMessage, error) {
	for _, name := range []string{"schedule_create", "schedule_list", "schedule_view", "schedule_edit"} {
		found := false
		for _, spec := range specs {
			if spec.Name == name {
				found = true
				break
			}
		}
		if !found {
			return session.AssistantMessage{}, errors.New("native Schedule tool missing: " + name)
		}
	}
	for _, message := range messages {
		if message.Role == "user" && strings.Contains(message.Content, "[SCHEDULE REMINDER") {
			return session.AssistantMessage{Content: "The scheduled reminder arrived."}, nil
		}
	}
	return session.AssistantMessage{}, errors.New("Schedule followup absent from model surface")
}

type scheduleToolTurnModel struct{}

func (scheduleToolTurnModel) Complete(_ context.Context, messages []agent.Message, _ []agent.ToolSpec) (session.AssistantMessage, error) {
	for _, message := range messages {
		if message.Role == "user" && strings.Contains(message.Content, "[SCHEDULE REMINDER") {
			return session.AssistantMessage{Content: "Scheduled followup completed."}, nil
		}
	}
	for _, message := range messages {
		if message.Role == "tool" && message.ToolName == "schedule_create" {
			return session.AssistantMessage{Content: "Reminder created."}, nil
		}
	}
	return session.AssistantMessage{ToolCalls: []session.ToolRequest{{
		ID: "schedule-call", Name: "schedule_create",
		Arguments: `{"title":"Check","prompt":"Check the report","rrule":"DTSTART:` + time.Now().UTC().Add(2*time.Second).Format("20060102T150405Z") + `\nRRULE:FREQ=DAILY;COUNT=1","target":"this_session"}`,
	}}}, nil
}

func TestNativeScheduleModelCreatesThenReceivesReminder(t *testing.T) {
	ctx := context.Background()
	journal := &nativeRuntimeStore{}
	store := &workerScheduleStore{document: schedule.Document{Version: 1}}
	worker := &NativeRuntime{}
	service, err := schedule.NewService(store, worker.DeliverSchedule)
	if err != nil {
		t.Fatal(err)
	}
	worker.Factory = nativeRuntimeTestFactory(t, journal, scheduleToolTurnModel{})
	worker.Factory.Schedule = service
	worker.Identity = assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "native-schedule-tool", Workspace: "/workspace"}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := worker.SubmitText(ctx, "Remind me to check the report"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(8 * time.Second)
	for {
		journal.mu.Lock()
		events := append([]sessionformat.Event(nil), journal.events...)
		journal.mu.Unlock()
		var created, delivered, followup bool
		for _, event := range events {
			if event.Type == "tool/call" && strings.Contains(string(event.Data), `"name":"schedule_create"`) {
				created = true
			}
			if event.Type == "user/message" && strings.Contains(string(event.Data), `"kind":"schedule"`) {
				delivered = true
			}
			if event.Type == "assistant/message" && strings.Contains(string(event.Data), "Scheduled followup completed") {
				followup = true
			}
		}
		if created && delivered && followup {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("model-to-schedule-to-model incomplete: create=%v delivery=%v followup=%v lastErr=%v", created, delivered, followup, worker.LastError())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestNativeScheduleTimerDeliversIntoOriginalSession(t *testing.T) {
	ctx := context.Background()
	journal := &nativeRuntimeStore{}
	store := &workerScheduleStore{document: schedule.Document{Version: 1}}
	worker := &NativeRuntime{}
	service, err := schedule.NewService(store, worker.DeliverSchedule)
	if err != nil {
		t.Fatal(err)
	}
	worker.Factory = nativeRuntimeTestFactory(t, journal, scheduledTurnModel{})
	worker.Factory.Schedule = service
	worker.Identity = assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "native-schedule", Workspace: "/workspace"}
	if err := worker.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if _, err := service.Create(ctx, func(now time.Time) (schedule.Record, error) {
		return schedule.NewAfter("reminder-1", "Reminder", "Check the report", 1, now)
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(8 * time.Second)
	for {
		journal.mu.Lock()
		events := append([]sessionformat.Event(nil), journal.events...)
		journal.mu.Unlock()
		var sourceFound, answerFound bool
		for _, event := range events {
			if event.Type == "user/message" && strings.Contains(string(event.Data), `"kind":"schedule"`) {
				sourceFound = true
			}
			if event.Type == "assistant/message" && strings.Contains(string(event.Data), "scheduled reminder arrived") {
				answerFound = true
			}
		}
		if sourceFound && answerFound {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("scheduled turn missing: source=%v answer=%v events=%d lastErr=%v", sourceFound, answerFound, len(events), worker.LastError())
		case <-time.After(10 * time.Millisecond):
		}
	}
	tasks, err := store.Load(ctx)
	if err != nil || len(tasks.Tasks) != 1 || tasks.Tasks[0].Status != "ended" || tasks.Tasks[0].LastDelivery == nil {
		t.Fatalf("Schedule receipt = %+v, %v", tasks, err)
	}
}

func TestNativeHostScheduleColdResumesOriginalSessionWithoutCrossDelivery(t *testing.T) {
	ctx := context.Background()
	stores := map[string]*nativeRuntimeStore{"first": {}, "second": {}}
	store := &workerScheduleStore{document: schedule.Document{Version: 1}}
	host := &NativeHost{UserID: "alice", StartedAt: time.Now().UTC()}
	service, err := schedule.NewHostService(store, host.PrepareSchedule, host.DeliverSchedule)
	if err != nil {
		t.Fatal(err)
	}
	host.Schedule = service
	factories := make(map[string]assembly.NativeFactory, len(stores))
	for id, journal := range stores {
		factory := nativeRuntimeTestFactory(t, journal, scheduledTurnModel{})
		factory.Schedule = service
		factories[id] = factory
	}
	host.Build = func(id string, _ bool) (*NativeRuntime, error) {
		return &NativeRuntime{
			Factory: factories[id], HostOwnsSchedule: true,
			Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: id, Workspace: "/workspace"},
		}, nil
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer host.Close(ctx)
	first, err := host.Activate(ctx, "first", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Activate(ctx, "second", false); err != nil {
		t.Fatal(err)
	}
	if err := host.StopSession(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ sessionID, id string }{{"first", "reminder-first"}, {"second", "reminder-second"}} {
		item := item
		if _, err := service.CreateFor(ctx, item.sessionID, func(now time.Time) (schedule.Record, error) {
			return schedule.NewAfter(item.id, item.id, item.id, 1, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	beforeDelivery, err := host.Activity()
	if err != nil || beforeDelivery.Idle {
		t.Fatalf("Host with active reminders appeared idle: %+v, %v", beforeDelivery, err)
	}
	deadline := time.After(8 * time.Second)
	for {
		complete := true
		for sessionID, journal := range stores {
			journal.mu.Lock()
			events := append([]sessionformat.Event(nil), journal.events...)
			journal.mu.Unlock()
			var delivered, answered bool
			for _, event := range events {
				if event.Type == "user/message" && strings.Contains(string(event.Data), `"kind":"schedule"`) && strings.Contains(string(event.Data), "reminder-"+sessionID) {
					delivered = true
				}
				if event.Type == "assistant/message" && strings.Contains(string(event.Data), "scheduled reminder arrived") {
					answered = true
				}
				if event.Type == "user/message" && strings.Contains(string(event.Data), "reminder-") && !strings.Contains(string(event.Data), "reminder-"+sessionID) {
					t.Fatalf("Schedule crossed Session boundary into %s: %s", sessionID, event.Data)
				}
			}
			complete = complete && delivered && answered
		}
		if complete {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Host did not deliver both reminders to their original Sessions")
		case <-time.After(10 * time.Millisecond):
		}
	}
	resumed, found := host.Session("first")
	if !found || resumed == first {
		t.Fatal("Host did not cold-resume first Session for its reminder")
	}
	receiptDeadline := time.After(5 * time.Second)
	for {
		document, err := store.Load(ctx)
		if err != nil || len(document.Tasks) != 2 {
			t.Fatalf("Host Schedule persistence = %+v, %v", document, err)
		}
		committed := true
		for _, task := range document.Tasks {
			committed = committed && task.LastDelivery != nil && task.Status == "ended"
		}
		if committed {
			break
		}
		select {
		case <-receiptDeadline:
			t.Fatalf("Host Schedule receipts were not committed after Session append: %+v", document)
		case <-time.After(10 * time.Millisecond):
		}
	}
	afterDelivery, err := host.Activity()
	if err != nil || !afterDelivery.Since.After(beforeDelivery.Since) {
		t.Fatalf("cold Schedule delivery did not refresh Host activity: before=%+v after=%+v err=%v", beforeDelivery, afterDelivery, err)
	}
}

func TestNativeHostActivityDoesNotMissColdOneShotDelivery(t *testing.T) {
	ctx := context.Background()
	journal := &nativeRuntimeStore{}
	store := &workerScheduleStore{document: schedule.Document{Version: 1}}
	host := &NativeHost{UserID: "alice", StartedAt: time.Now().Add(-25 * time.Hour)}
	deliverEntered := make(chan struct{})
	releaseDelivery := make(chan struct{})
	service, err := schedule.NewHostService(store, func(context.Context, string) error { return nil }, func(ctx context.Context, sessionID, text string) (string, error) {
		close(deliverEntered)
		select {
		case <-releaseDelivery:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return host.DeliverSchedule(ctx, sessionID, text)
	})
	if err != nil {
		t.Fatal(err)
	}
	host.Schedule = service
	host.Build = func(id string, _ bool) (*NativeRuntime, error) {
		factory := nativeRuntimeTestFactory(t, journal, scheduledTurnModel{})
		factory.Schedule = service
		return &NativeRuntime{Factory: factory, HostOwnsSchedule: true,
			Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: id, Workspace: "/workspace"}}, nil
	}
	initial, err := host.Activate(ctx, "first", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.StopSession(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	baseline, err := host.Activity()
	if err != nil || baseline.Idle == false {
		t.Fatalf("cold Host baseline = %+v, %v", baseline, err)
	}
	record, err := schedule.NewAfter("one-shot", "Reminder", "Check", 1, time.Now().Add(-2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.document.Tasks = []schedule.Task{{SessionID: "first", Record: record, Status: "active", DeliveryHistory: schedule.History{Records: []schedule.Receipt{}}}}
	store.mu.Unlock()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer host.Close(ctx)
	defer func() {
		select {
		case <-releaseDelivery:
		default:
			close(releaseDelivery)
		}
	}()
	select {
	case <-deliverEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("due Schedule did not enter delivery")
	}
	type result struct {
		activity sessionformat.ActivitySnapshot
		err      error
	}
	activityDone := make(chan result, 1)
	activityStarted := make(chan struct{})
	go func() {
		close(activityStarted)
		activity, err := host.Activity()
		activityDone <- result{activity, err}
	}()
	<-activityStarted
	select {
	case activity := <-activityDone:
		t.Fatalf("activity returned while Schedule held its delivery lock: %+v", activity)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseDelivery)
	select {
	case got := <-activityDone:
		if got.err != nil || !got.activity.Since.After(baseline.Since) {
			t.Fatalf("cold delivery was omitted from activity snapshot: before=%+v after=%+v err=%v", baseline, got.activity, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("activity did not finish after Schedule delivery")
	}
	resumed, found := host.Session("first")
	if !found || resumed == initial {
		t.Fatal("Schedule did not cold-resume the original Session")
	}
}
