package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostScheduleFileStoreSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".harness", "schedule", "schedule.json")
	store := FileStore{Path: path}
	ctx := context.Background()
	empty, err := store.Load(ctx)
	if err != nil || empty.Version != 1 || len(empty.Tasks) != 0 {
		t.Fatalf("empty Schedule = %+v, %v", empty, err)
	}
	document := Document{Version: 1, Tasks: []Task{{SessionID: "first", Record: Record{ID: "task"}, Status: "active"}}}
	if err := store.Save(ctx, document); err != nil {
		t.Fatal(err)
	}
	loaded, err := (FileStore{Path: path}).Load(ctx)
	if err != nil || len(loaded.Tasks) != 1 || loaded.Tasks[0].SessionID != "first" {
		t.Fatalf("reopened Schedule = %+v, %v", loaded, err)
	}
	if mode := fileMode(t, path); mode != 0600 {
		t.Fatalf("Schedule mode = %o", mode)
	}
}

func TestHostScheduleFileStoreDistinguishesAbsentFromMalformedOptionalHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	store := FileStore{Path: path}
	record, err := NewAfter("task", "Check", "Prompt", 60, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]any{"sessionId": "original", "record": record, "status": "inactive"}
	tests := []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing owner", func(task map[string]any) { delete(task, "sessionId") }},
		{"null history", func(task map[string]any) { task["deliveryHistory"] = nil }},
		{"empty history object", func(task map[string]any) { task["deliveryHistory"] = map[string]any{} }},
		{"null history records", func(task map[string]any) {
			task["deliveryHistory"] = map[string]any{"records": nil, "earlierRecordsUnavailable": false}
		}},
		{"missing history flag", func(task map[string]any) { task["deliveryHistory"] = map[string]any{"records": []any{}} }},
		{"null history flag", func(task map[string]any) {
			task["deliveryHistory"] = map[string]any{"records": []any{}, "earlierRecordsUnavailable": nil}
		}},
		{"null last delivery", func(task map[string]any) { task["lastDelivery"] = nil }},
		{"last delivery prompt", func(task map[string]any) {
			task["lastDelivery"] = map[string]any{"scheduledAt": record.ScheduledAt, "deliveredAt": record.ScheduledAt, "messageId": "message-1", "prompt": ""}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := make(map[string]any, len(base)+1)
			for key, value := range base {
				task[key] = value
			}
			test.change(task)
			before, err := json.Marshal(map[string]any{"version": 1, "tasks": []any{task}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(context.Background()); err == nil {
				t.Fatal("accepted malformed saved Schedule task")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("rejected Schedule was rewritten: %v", err)
			}
		})
	}
	delete(base, "status")
	legacy, err := json.Marshal(map[string]any{"version": 1, "tasks": []any{base}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background())
	if err != nil || len(loaded.Tasks) != 1 || loaded.Tasks[0].Status != "active" || loaded.Tasks[0].DeliveryHistory.Records != nil {
		t.Fatalf("valid legacy task = %+v, %v", loaded, err)
	}
	if err := store.Save(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil || bytes.Contains(saved, []byte(`"deliveryHistory"`)) {
		t.Fatalf("absent legacy history became a present field: %s, %v", saved, err)
	}
	loaded.Tasks[0].LastDelivery = &Receipt{ScheduledAt: record.ScheduledAt, DeliveredAt: record.ScheduledAt, MessageID: "legacy-message"}
	if err := store.Save(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	saved, err = os.ReadFile(path)
	if err != nil || bytes.Contains(saved, []byte(`"deliveryHistory"`)) {
		t.Fatalf("legacy receipt invented a history field: %s, %v", saved, err)
	}
	loaded, err = store.Load(context.Background())
	if err != nil || loaded.Tasks[0].LastDelivery == nil || loaded.Tasks[0].LastDelivery.MessageID != "legacy-message" {
		t.Fatalf("legacy last receipt did not survive reopen: %+v, %v", loaded, err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestHostScheduleFileStoreRejectsUnknownAndTrailingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	store := FileStore{Path: path}
	for _, raw := range []string{
		`{"version":1,"tasks":[],"unexpected":true}`,
		`{"version":1}`,
		`{"version":1,"tasks":null}`,
		"{\"version\":1,\"tasks\":[]}\n{}",
		`{"version":2,"tasks":[]}`,
	} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(context.Background()); err == nil {
			t.Fatalf("accepted invalid Schedule document %q", raw)
		}
	}
}

func TestHostScheduleFileStoreCancelledSaveKeepsPriorDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	store := FileStore{Path: path}
	original := Document{Version: 1, Tasks: []Task{{SessionID: "first", Record: Record{ID: "one"}, Status: "active"}}}
	if err := store.Save(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Save(ctx, Document{Version: 1, Tasks: []Task{{SessionID: "second", Record: Record{ID: "two"}, Status: "active"}}}); err == nil {
		t.Fatal("cancelled Schedule save unexpectedly succeeded")
	}
	loaded, err := store.Load(context.Background())
	if err != nil || len(loaded.Tasks) != 1 || loaded.Tasks[0].SessionID != "first" {
		t.Fatalf("cancelled Save changed Schedule file: %+v, %v", loaded, err)
	}
}

// This envelope and task shape come from the pinned upstream Schedule README.
func TestScheduleDomainReadsUpstreamV2AndRoundTripsTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dsh", "storages", "schedule.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	fixture := `{"unit":{"name":"schedule","version":2},"global":null,"tables":{"tasks":{"deploy-check":{
		"record":{"id":"deploy-check","kind":"cron","title":"Deploy check","prompt":"Check the deployment.","expression":"0 9 * * 1-5","timeZone":"Asia/Shanghai","scheduledAt":"2099-01-01T01:00:00.000Z"},
		"target":{"kind":"session","sessionId":"existing-session"},"status":"active","involvedSessionIds":["existing-session"],
		"createdAt":"2026-10-21T00:00:00.000Z","updatedAt":"2026-10-21T00:00:00.000Z",
		"deliveryHistory":{"records":[],"earlierRecordsUnavailable":false,"earlierRecordsPruned":false}
	}}}}`
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	store := FileStore{Path: path}
	doc, err := store.Load(context.Background())
	if err != nil || len(doc.Tasks) != 1 || doc.Tasks[0].Target == nil || doc.Tasks[0].Target.SessionID != "existing-session" {
		t.Fatalf("decoded %+v, %v", doc, err)
	}
	if err := validateDocument(doc); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored scheduleDomain
	if err := json.Unmarshal(data, &stored); err != nil || stored.Unit != (scheduleUnit{Name: "schedule", Version: 2}) {
		t.Fatalf("envelope: %v %v", stored.Unit, err)
	}
	var taskTable map[string]Task
	if err := json.Unmarshal(stored.Tables.Tasks, &taskTable); err != nil || taskTable["deploy-check"].Target == nil ||
		taskTable["deploy-check"].SessionID != "" {
		t.Fatalf("unexpected serialized task table: %s, %v", stored.Tables.Tasks, err)
	}
}
