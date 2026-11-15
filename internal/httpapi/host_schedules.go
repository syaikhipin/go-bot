package httpapi

import (
	"net/http"

	"go-harness/harness/schedule"
)

func serveHostSchedules(w http.ResponseWriter, r *http.Request, options NativeWorkerHTTPOptions) {
	if options.ReadSchedules == nil {
		writeSessionJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "schedules_unavailable"})
		return
	}
	document, err := options.ReadSchedules(r.Context())
	if err != nil {
		writeSessionJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "schedules_unavailable"})
		return
	}
	type row struct {
		Record       schedule.Record   `json:"record"`
		Status       string            `json:"status"`
		SessionID    string            `json:"sessionId,omitempty"`
		Target       *schedule.Target  `json:"target,omitempty"`
		LastDelivery *schedule.Receipt `json:"lastDelivery,omitempty"`
	}
	tasks := make([]row, 0, len(document.Tasks))
	for _, task := range document.Tasks {
		tasks = append(tasks, row{task.Record, task.Status, task.SessionID, task.Target, task.LastDelivery})
	}
	writeSessionJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}
