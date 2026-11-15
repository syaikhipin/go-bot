package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go-harness/harness/schedule"
)

const hostSchedulePath = "/v1/host/schedules"

func manageHostSchedules(w http.ResponseWriter, r *http.Request, service *schedule.Service) {
	w.Header().Set("Cache-Control", "no-store")
	path := strings.TrimPrefix(r.URL.Path, hostSchedulePath)
	var result any
	var err error
	status := http.StatusOK
	switch {
	case path == "" && r.Method == http.MethodGet:
		var tasks []schedule.Entry
		tasks, err = service.Catalog()
		result = map[string]any{"tasks": tasks}
	case path == "" && r.Method == http.MethodPost:
		var input schedule.CreateRequest
		if !decodeScheduleRequest(w, r, &input) {
			return
		}
		result, err = service.CreateTask(r.Context(), "", input)
		status = http.StatusCreated
	case path == "/list" && r.Method == http.MethodGet:
		query := r.URL.Query()
		limit, offset := 100, 0
		var active *bool
		for key, target := range map[string]*int{"limit": &limit, "offset": &offset} {
			if values, present := query[key]; present {
				num, parseErr := strconv.Atoi(values[0])
				if parseErr != nil {
					writeScheduleError(w, schedule.ErrInvalid)
					return
				}
				*target = num
			}
		}
		if values, present := query["active"]; present {
			if values[0] != "true" && values[0] != "false" {
				writeScheduleError(w, schedule.ErrInvalid)
				return
			}
			value := values[0] == "true"
			active = &value
		}
		result, err = service.ListTasks(active, limit, offset)
	case path == "/limits" && r.Method == http.MethodGet:
		result = map[string]int{"newSessionIntervalSeconds": service.DeliveryLimits().NewSessionIntervalSeconds}
	case path == "/ended" && r.Method == http.MethodDelete:
		var deleted int
		deleted, err = service.DeleteInactive(r.Context())
		result = map[string]int{"deleted": deleted}
	case path == "/stream" && r.Method == http.MethodGet:
		streamScheduleChanges(w, r, service)
		return
	default:
		id, action, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
		if id == "" || strings.TrimSpace(id) != id {
			http.NotFound(w, r)
			return
		}
		switch {
		case action == "" && r.Method == http.MethodGet:
			result, err = service.ViewTask(id)
		case action == "" && r.Method == http.MethodPatch:
			var input schedule.EditRequest
			if !decodeScheduleRequest(w, r, &input) {
				return
			}
			if input.ID != "" && input.ID != id {
				writeScheduleError(w, schedule.ErrInvalid)
				return
			}
			input.ID = id
			result, err = service.EditTask(r.Context(), "", input)
			if input.Action == "delete" {
				result = map[string]any{"id": id, "deleted": true}
			}
		case action == "run" && r.Method == http.MethodPost:
			result, err = service.RunNow(r.Context(), id)
		case action == "history" && r.Method == http.MethodGet:
			limit, parseErr := strconv.Atoi(r.URL.Query().Get("limit"))
			if parseErr != nil || limit < 1 || limit > 100 {
				writeScheduleError(w, schedule.ErrInvalid)
				return
			}
			var before *string
			if values, present := r.URL.Query()["before"]; present {
				before = &values[0]
			}
			var history schedule.HistoryPage
			history, err = service.History(id, limit, before)
			switch history.Code {
			case "schedule_not_found":
				err = schedule.ErrNotFound
			case "delivery_cursor_not_found":
				err = errors.New(history.Code)
			}
			result = history
		default:
			http.NotFound(w, r)
			return
		}
	}
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	writeSessionJSON(w, status, result)
}

func decodeScheduleRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeScheduleError(w, schedule.ErrInvalid)
		return false
	}
	return true
}

func writeScheduleError(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "schedule_unavailable"
	body := map[string]string{}
	switch {
	case errors.Is(err, schedule.ErrNotFound), err.Error() == "delivery_cursor_not_found":
		status, code = http.StatusNotFound, err.Error()
	case errors.Is(err, schedule.ErrConflict), errors.Is(err, schedule.ErrExecutionPending):
		status, code = http.StatusConflict, err.Error()
	case errors.Is(err, schedule.ErrInvalid), errors.Is(err, os.ErrNotExist):
		status, code = http.StatusBadRequest, schedule.ErrInvalid.Error()
		if err != schedule.ErrInvalid {
			body["detail"] = err.Error()
		}
	}
	body["error"] = code
	writeSessionJSON(w, status, body)
}

func streamScheduleChanges(w http.ResponseWriter, r *http.Request, service *schedule.Service) {
	changed, done := service.Changes()
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	if _, err := fmt.Fprint(w, "event: schedule/changed\ndata: {}\n\n"); err != nil || controller.Flush() != nil {
		return
	}
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		var frame string
		select {
		case <-r.Context().Done():
			return
		case <-done:
			return
		case <-changed:
			changed, _ = service.Changes()
			frame = "event: schedule/changed\ndata: {}\n\n"
		case <-heartbeat.C:
			frame = ": heartbeat\n\n"
		}
		if _, err := fmt.Fprint(w, frame); err != nil || controller.Flush() != nil {
			return
		}
	}
}
