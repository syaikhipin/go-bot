package plugins

import (
	"encoding/json"
	"errors"
	"go-bot/plugin"
	"io"
	"net/http"
	"strings"
)

func (m *Manager) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond := func(code int, value any) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(value)
		}
		decode := func(value any) error {
			d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
			d.DisallowUnknownFields()
			if err := d.Decode(value); err != nil {
				return err
			}
			if !errors.Is(d.Decode(new(any)), io.EOF) {
				return errors.New("expected one JSON object")
			}
			return nil
		}
		fail := func(err error) {
			respond(http.StatusUnprocessableEntity, map[string]string{"error": "plugin_operation_failed", "message": m.redact(err.Error())})
		}
		suffix := strings.TrimPrefix(r.URL.Path, plugin.InternalPath)
		switch {
		case suffix == "/authorization" || strings.HasPrefix(suffix, "/authorization/"):
			m.authorizationHTTP(w, r, respond, decode)
		case r.Method == http.MethodGet && suffix == "":
			status, err := m.Status(r.Context())
			if err != nil {
				fail(err)
				return
			}
			respond(200, status)
		case r.Method == http.MethodPut && suffix == "/settings":
			var settings plugin.RuntimeSettings
			if err := decode(&settings); err != nil {
				respond(400, map[string]string{"error": "invalid_plugin_settings"})
				return
			}
			status, err := m.Apply(r.Context(), settings)
			if err != nil {
				fail(err)
				return
			}
			respond(200, status)
		case r.Method == http.MethodPost && suffix == "/packages/inspect":
			var input struct {
				Name   string        `json:"name"`
				Source plugin.Source `json:"source"`
			}
			if err := decode(&input); err != nil {
				respond(400, map[string]string{"error": "invalid_plugin_package"})
				return
			}
			info, err := m.InspectPackage(r.Context(), input.Name, input.Source)
			if err != nil {
				fail(err)
				return
			}
			respond(200, info)
		case r.Method == http.MethodPost && (suffix == "/packages/remove" || suffix == "/packages/reinstall"):
			var input struct {
				Name     string                 `json:"name"`
				Settings plugin.RuntimeSettings `json:"settings"`
			}
			if err := decode(&input); err != nil {
				respond(400, map[string]string{"error": "invalid_plugin_package"})
				return
			}
			var status Status
			var err error
			if suffix == "/packages/remove" {
				status, err = m.Remove(r.Context(), input.Name, input.Settings)
			} else {
				status, err = m.Reinstall(r.Context(), input.Name, input.Settings)
			}
			if err != nil {
				fail(err)
				return
			}
			respond(200, status)
		case r.Method == http.MethodPost && suffix == "/runtime/restart":
			var input struct {
				Settings *plugin.RuntimeSettings `json:"settings,omitempty"`
			}
			if r.Body != nil && r.ContentLength != 0 {
				if err := decode(&input); err != nil {
					respond(400, map[string]string{"error": "invalid_plugin_settings"})
					return
				}
			}
			status, err := m.restart(r.Context(), input.Settings)
			if err != nil {
				fail(err)
				return
			}
			respond(200, status)
		default:
			http.NotFound(w, r)
		}
	})
}
