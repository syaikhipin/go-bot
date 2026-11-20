package mcpclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	mcpconfig "go-bot/mcp"
)

// Handler is private Host control, protected by the existing Host token.
func (m *Manager) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(code int, value any) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(value)
		}
		failure := func(err error) {
			write(http.StatusUnprocessableEntity, map[string]any{"error": "mcp_operation_failed", "message": m.redact(err.Error()), "status": m.Status()})
		}
		suffix := strings.TrimPrefix(r.URL.Path, mcpconfig.InternalPath)
		decode := func(value any) bool {
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
			decoder.DisallowUnknownFields()
			if decoder.Decode(value) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
				write(400, map[string]string{"error": "invalid_mcp_request"})
				return false
			}
			return true
		}
		switch {
		case suffix == "/oauth/callback" && r.Method == http.MethodPost:
			var input mcpconfig.OAuthCallback
			if !decode(&input) {
				return
			}
			view, err := m.ReceiveAuthorization(input)
			if err != nil {
				failure(err)
				return
			}
			write(200, view)
		case suffix == "" && r.Method == http.MethodGet:
			write(http.StatusOK, m.Status())
		case (r.Method == http.MethodPost || r.Method == http.MethodDelete) && strings.HasPrefix(suffix, "/servers/"):
			name, action, found := strings.Cut(strings.TrimPrefix(suffix, "/servers/"), "/")
			if !mcpconfig.ValidServerName(name) {
				write(http.StatusBadRequest, map[string]string{"error": "invalid_mcp_server"})
				return
			}
			if !found {
				http.NotFound(w, r)
				return
			}
			if r.Method == http.MethodDelete {
				var status mcpconfig.Status
				var err error
				switch action {
				case "authorization":
					if id := r.URL.Query().Get("id"); id != "" {
						err = m.CancelAuthorizationAttempt(r.Context(), name, id)
						status = m.Status()
					} else {
						status, err = m.CancelAuthorization(r.Context(), name)
					}
				case "grant":
					status, err = m.DisconnectAuthorization(r.Context(), name)
				default:
					http.NotFound(w, r)
					return
				}
				if err != nil {
					failure(err)
					return
				}
				write(200, status)
				return
			}
			if action == "authorize" {
				var input struct {
					RedirectURL string `json:"redirectUrl"`
				}
				if !decode(&input) {
					return
				}
				view, err := m.Authorize(r.Context(), name, input.RedirectURL)
				if err != nil {
					failure(err)
					return
				}
				write(200, view)
				return
			}
			if action != "restart" && action != "probe" {
				http.NotFound(w, r)
				return
			}
			if action == "probe" {
				result, err := m.Probe(r.Context(), name)
				if err != nil {
					failure(err)
					return
				}
				write(http.StatusOK, result)
				return
			}
			status, err := m.Restart(r.Context(), name)
			if err != nil {
				failure(err)
				return
			}
			write(http.StatusOK, status)
		default:
			http.NotFound(w, r)
		}
	})
}
