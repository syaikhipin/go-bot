package schedule

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScheduleStoredReceiptAdmissionMatchesUpstream(t *testing.T) {
	record, err := NewAfter("task", "Check", "Prompt", 60, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	receipt := Receipt{
		ScheduledAt: "2026-10-12T00:01:00.000Z",
		DeliveredAt: "2026-10-12T00:01:01.000Z",
		MessageID:   "message-1",
	}
	for _, test := range []struct {
		name      string
		task      Task
		wantError bool
	}{
		{"known empty", Task{SessionID: "session", Record: record, Status: "inactive", DeliveryHistory: History{Records: []Receipt{}}}, false},
		{"legacy receipt without history", Task{SessionID: "session", Record: record, Status: "inactive", LastDelivery: &receipt}, false},
		{"known empty with last receipt", Task{SessionID: "session", Record: record, Status: "inactive", LastDelivery: &receipt, DeliveryHistory: History{Records: []Receipt{}}}, true},
		{"history message ID has outer whitespace", Task{SessionID: "session", Record: record, Status: "inactive", LastDelivery: &receipt, DeliveryHistory: History{Records: []Receipt{{ScheduledAt: receipt.ScheduledAt, DeliveredAt: receipt.DeliveredAt, MessageID: " message-1"}}}}, true},
		{"last message ID has outer whitespace", Task{SessionID: "session", Record: record, Status: "inactive", LastDelivery: &Receipt{ScheduledAt: receipt.ScheduledAt, DeliveredAt: receipt.DeliveredAt, MessageID: "message-1 "}}, true},
		{"last receipt has history-only prompt", Task{SessionID: "session", Record: record, Status: "inactive", LastDelivery: &Receipt{ScheduledAt: receipt.ScheduledAt, DeliveredAt: receipt.DeliveredAt, MessageID: receipt.MessageID, Prompt: "Prompt"}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateDocument(Document{Version: 1, Tasks: []Task{test.task}})
			if (err != nil) != test.wantError {
				t.Fatalf("admission error = %v; want error %v", err, test.wantError)
			}
		})
	}
}

func TestHostScheduleRejectsMalformedHistoryWithoutOverwritingFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schedule.json")
	store := FileStore{Path: path}
	record, err := NewAfter("task", "Check", "Prompt", 60, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	document := Document{Version: 1, Tasks: []Task{{
		SessionID: "session", Record: record, Status: "inactive",
		LastDelivery:    &Receipt{ScheduledAt: record.ScheduledAt, DeliveredAt: record.ScheduledAt, MessageID: "message-1"},
		DeliveryHistory: History{Records: []Receipt{}},
	}}}
	if err := store.Save(ctx, document); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHostService(store, noOpHostPrepare, func(context.Context, string, string) (string, error) {
		t.Fatal("invalid Schedule must never deliver")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err == nil {
		t.Fatal("Host opened a Schedule document with contradictory history")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("failed Host startup changed the Schedule document: %v", err)
	}
}
