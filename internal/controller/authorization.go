package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go-bot/plugin"
)

func (m *Manager) authorizationHTTP(w http.ResponseWriter, r *http.Request, id string) {
	suffix := strings.TrimPrefix(r.URL.Path, plugin.AuthorizationPath)
	if suffix == "" {
		if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
	} else if suffix != "/disconnect" || r.Method != http.MethodPost {
		attempt, action, _ := strings.Cut(strings.TrimPrefix(suffix, "/"), "/")
		_, err := uuid.Parse(attempt)
		if err != nil || !((action == "" && (r.Method == http.MethodGet || r.Method == http.MethodDelete)) || (action == "answer" && r.Method == http.MethodPost)) {
			http.NotFound(w, r)
			return
		}
	}
	// No account management lock is held while a credential flow progresses;
	// answering and cancelling must remain independent requests.
	record, err := m.Store.Get(r.Context(), id)
	if errors.Is(err, ErrAccountNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil || record.DesiredState != running || record.HostEndpoint == "" {
		httpError(w, http.StatusConflict, "host_not_running")
		return
	}
	var input any
	if r.Method == http.MethodPost || (suffix == "" && r.Method == http.MethodDelete) {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
		d.DisallowUnknownFields()
		if r.Method == http.MethodDelete {
			input = &struct {
				Key string `json:"key"`
			}{}
		} else if suffix == "/disconnect" {
			input = &struct {
				Key string `json:"key"`
			}{}
		} else if suffix == "" {
			input = &plugin.AuthorizationStart{}
		} else {
			input = &plugin.AuthorizationAnswer{}
		}
		if d.Decode(input) != nil || !errors.Is(d.Decode(new(any)), io.EOF) {
			httpError(w, 400, "invalid_authorization_request")
			return
		}
	}
	var result json.RawMessage
	if err = m.requestPlugins(r.Context(), record, r.Method, "/authorization"+suffix, input, &result); err != nil {
		status := http.StatusBadGateway
		var upstream *extensionHTTPError
		if errors.As(err, &upstream) {
			status = upstream.Status
		}
		writeJSON(w, status, map[string]string{"error": "plugin_authorization_failed", "message": err.Error()})
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost && suffix == "" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, result)
}
