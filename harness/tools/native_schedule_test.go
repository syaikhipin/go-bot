package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go-harness/harness/schedule"
)

type scheduleToolStore struct {
	mu       sync.Mutex
	document schedule.Document
}

func (s *scheduleToolStore) Load(context.Context) (schedule.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(s.document)
	var document schedule.Document
	_ = json.Unmarshal(data, &document)
	return document, nil
}

func (s *scheduleToolStore) Save(_ context.Context, document schedule.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(document)
	return json.Unmarshal(data, &s.document)
}

func TestNativeScheduleUpstreamToolsManageHostTasks(t *testing.T) {
	ctx := t.Context()
	store := schedule.FileStore{Path: filepath.Join(t.TempDir(), "schedule.json")}
	service, err := schedule.NewHostService(store, func(context.Context, string) error { return nil },
		func(context.Context, string, string) (string, error) { return "", errors.New("unexpected delivery") })
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	call := func(sessionID, name, input string) string {
		t.Helper()
		tool := NativeSchedule{Service: service, SessionID: sessionID, Name: name}
		if !json.Valid(tool.Spec().Parameters) {
			t.Fatal("invalid tool schema")
		}
		result, err := tool.Execute(ctx, json.RawMessage(input))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	created := call("origin", "schedule_create", `{"title":"Check build","prompt":"Review logs","rrule":"DTSTART:20990101T090000Z\nRRULE:FREQ=DAILY","target":"this_session"}`)
	var task struct {
		ID        string
		UpdatedAt string
	}
	if err := json.Unmarshal([]byte(created), &task); err != nil || task.ID == "" || task.UpdatedAt == "" {
		t.Fatalf("create result: %s, %v", created, err)
	}
	if result := call("other", "schedule_list", `{"active":true,"limit":1}`); !strings.Contains(result, task.ID) {
		t.Fatalf("other Session could not list Host task: %s", result)
	}
	if result := call("other", "schedule_view", `{"id":"`+task.ID+`"}`); !strings.Contains(result, task.UpdatedAt) {
		t.Fatalf("view lost edit revision: %s", result)
	}
	if result := call("other", "schedule_edit", `{"id":"`+task.ID+`","action":"update","active":false,"expectedUpdatedAt":"`+task.UpdatedAt+`"}`); !strings.Contains(result, `"status":"paused"`) {
		t.Fatalf("edit failed: %s", result)
	}
	if result := call("other", "schedule_edit", `{"id":"`+task.ID+`","action":"delete","expectedUpdatedAt":"`+task.UpdatedAt+`"}`); !strings.Contains(result, "schedule_conflict") {
		t.Fatalf("stale deletion went through: %s", result)
	}
	if result := call("other", "schedule_edit", `{"id":"`+task.ID+`","action":"delete"}`); !strings.Contains(result, `"deleted":true`) {
		t.Fatalf("delete failed: %s", result)
	}
	delegated := NativeSchedule{Service: service, SessionID: "child", Name: "schedule_list", Delegated: true}
	if result, err := delegated.Execute(ctx, json.RawMessage(`{}`)); err != nil || !strings.Contains(result, "subagent_session") {
		t.Fatalf("delegated task access: %s, %v", result, err)
	}
}

func TestNativeScheduleUpdateKeepsOverdueEquivalentAtTarget(t *testing.T) {
	ctx := context.Background()
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	after, err := schedule.NewAfter("after", "Original after", "Check", 60, past)
	if err != nil {
		t.Fatal(err)
	}
	at, err := schedule.NewAt("at", "Original at", "Check", "2020-01-01T00:01:00Z", past)
	if err != nil {
		t.Fatal(err)
	}
	store := &scheduleToolStore{document: schedule.Document{Version: 1, Tasks: []schedule.Task{
		{Record: after, Status: "active"}, {Record: at, Status: "active"},
	}}}
	service, err := schedule.NewService(store, func(context.Context, string) (string, error) {
		return "", errors.New("delivery temporarily unavailable")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	for _, original := range []schedule.Record{after, at} {
		for _, target := range []any{
			original.ScheduledAt,
			"2020-01-01T08:01:00+08:00",
			map[string]string{"date": "2020-01-01", "time": "08:01:00", "time_zone": "Asia/Shanghai"},
		} {
			at, _ := json.Marshal(target)
			title := "Renamed"
			input := schedule.EditRequest{ID: original.ID, Action: "update", Patch: &schedule.Patch{Title: &title, Timing: &schedule.Timing{Kind: "at", At: at}}}
			if _, err := service.EditTask(ctx, "", input); err != nil {
				t.Fatalf("equivalent overdue %s update at %v: %v", original.Kind, target, err)
			}
		}
		got, found, err := service.LookupFor("", original.ID)
		if err != nil || !found || got.Record.Kind != original.Kind || got.Record.ScheduledAt != original.ScheduledAt || got.Record.Title != "Renamed" {
			t.Fatalf("equivalent overdue %s update changed timing: %+v, found=%t, err=%v", original.Kind, got, found, err)
		}
	}
	if _, err := service.EditTask(ctx, "", schedule.EditRequest{ID: "at", Action: "update", Patch: &schedule.Patch{Timing: &schedule.Timing{Kind: "at", At: json.RawMessage(`"2020-01-01T00:02:00Z"`)}}}); err == nil {
		t.Fatal("changed overdue target was accepted as an equivalent update")
	}
}

func TestNativeScheduleUpdateRetainsEquivalentWallRuleAlias(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	daily, err := schedule.NewDaily("daily", "Daily", "Check", schedule.LocalTimeInput{Time: "09:00:00", TimeZone: "America/New_York"}, start)
	if err != nil {
		t.Fatal(err)
	}
	weekly, err := schedule.NewWeekly("weekly", "Weekly", "Check", schedule.LocalTimeInput{Time: "09:00:00", TimeZone: "America/New_York", Weekdays: []int{1}}, start)
	if err != nil {
		t.Fatal(err)
	}
	cron, err := schedule.NewCron("cron", "Cron", "Check", schedule.CronInput{Expression: "0 9 * * *", TimeZone: "America/New_York"}, start)
	if err != nil {
		t.Fatal(err)
	}
	var tasks []schedule.Task
	for _, record := range []schedule.Record{daily, weekly, cron} {
		record.TimeZone = "US/Eastern" // Existing logs retain valid alias spelling.
		tasks = append(tasks, schedule.Task{Record: record, Status: "active"})
	}
	service, err := schedule.NewService(&scheduleToolStore{document: schedule.Document{Version: 1, Tasks: tasks}}, func(context.Context, string) (string, error) {
		return "", errors.New("delivery temporarily unavailable")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	for _, test := range []struct {
		record schedule.Record
		rule   string
	}{
		{daily, `"daily":{"time":"09:00:00","time_zone":"America/New_York"}`},
		{weekly, `"weekly":{"time":"09:00:00","time_zone":"America/New_York","weekdays":[1]}`},
		{cron, `"cron":{"expression":"0 9 * * *","time_zone":"America/New_York"}`},
	} {
		title := "Renamed"
		var timing schedule.Timing
		if err := json.Unmarshal([]byte(`{"kind":"`+test.record.Kind+`",`+test.rule+`}`), &timing); err != nil {
			t.Fatal(err)
		}
		if _, err := service.EditTask(ctx, "", schedule.EditRequest{ID: test.record.ID, Action: "update", Patch: &schedule.Patch{Title: &title, Timing: &timing}}); err != nil {
			t.Fatalf("equivalent %s update: %v", test.record.Kind, err)
		}
		got, found, err := service.LookupFor("", test.record.ID)
		if err != nil || !found || got.Record.TimeZone != "US/Eastern" || got.Record.ScheduledAt != test.record.ScheduledAt || got.Record.Title != "Renamed" {
			t.Fatalf("equivalent %s update rewrote alias or target: %+v, found=%t, err=%v", test.record.Kind, got, found, err)
		}
	}
}

func TestNativeScheduleUpdateDoesNotSearchForFutureOnEquivalentCron(t *testing.T) {
	ctx := context.Background()
	record := schedule.Record{
		ID: "cron", Kind: "cron", Title: "Reminder", Prompt: "Check", Expression: "0 0 30 2 *",
		TimeZone: "UTC", ScheduledAt: "2099-01-01T00:00:00.000Z",
	}
	service, err := schedule.NewService(&scheduleToolStore{document: schedule.Document{Version: 1, Tasks: []schedule.Task{{Record: record, Status: "active"}}}},
		func(context.Context, string) (string, error) { return "", errors.New("unexpected delivery") })
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	title := "Renamed"
	_, err = service.EditTask(ctx, "", schedule.EditRequest{ID: "cron", Action: "update", Patch: &schedule.Patch{Title: &title, Timing: &schedule.Timing{Kind: "cron", Cron: &schedule.CronInput{Expression: "0 0 30 2 *", TimeZone: "UTC"}}}})
	if err != nil {
		t.Fatalf("equivalent cron incorrectly searched for a new target: %v", err)
	}
	got, found, err := service.LookupFor("", "cron")
	if err != nil || !found || got.Record.Kind != "cron" || got.Record.Title != "Renamed" ||
		got.Record.Expression != record.Expression || got.Record.TimeZone != record.TimeZone || got.Record.ScheduledAt != record.ScheduledAt {
		t.Fatalf("equivalent cron changed its saved target: %+v, found=%t, err=%v", got, found, err)
	}
}
