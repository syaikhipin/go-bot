package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-bot/harness/schedule"
)

func TestNativeHostScheduleList(t *testing.T) {
	handler := NewNativeHostHTTP(nil, NativeWorkerHTTPOptions{
		ReadSchedules: func(context.Context) (schedule.Document, error) {
			return schedule.Document{Tasks: []schedule.Task{{
				Record: schedule.Record{ID: "task", Title: "Report", Kind: "after"},
				Status: "active", SessionID: "session",
				LastDelivery: &schedule.Receipt{MessageID: "message"},
			}}}, nil
		},
	})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/host/schedules", nil))
	var result struct {
		Tasks []struct {
			Record       schedule.Record   `json:"record"`
			Status       string            `json:"status"`
			SessionID    string            `json:"sessionId"`
			LastDelivery *schedule.Receipt `json:"lastDelivery"`
		} `json:"tasks"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Tasks) != 1 {
		t.Fatalf("schedule list: %d %s", w.Code, w.Body)
	}
	task := result.Tasks[0]
	if task.Record.ID != "task" || task.Status != "active" || task.SessionID != "session" || task.LastDelivery == nil || task.LastDelivery.MessageID != "message" || strings.Contains(w.Body.String(), "deliveryHistory") {
		t.Fatalf("unexpected task projection: %s", w.Body)
	}
}
