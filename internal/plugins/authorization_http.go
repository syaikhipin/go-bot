package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"go-bot/internal/authorization"
	"go-bot/plugin"
)

func (m *Manager) Authorization(ctx context.Context, method string, input any) (json.RawMessage, error) {
	switch method {
	case "list", "start", "get", "answer", "cancel", "cancelKey", "callback", "disconnect":
	default:
		return nil, failure("METHOD_NOT_FOUND", "Unknown authorization operation")
	}
	m.mu.RLock()
	runtime := m.runtime
	m.mu.RUnlock()
	if runtime == nil || !runtime.Alive() {
		return nil, failure("RUNTIME_UNAVAILABLE", "Plugin runtime is not running")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	value, err := m.surface.Handle(ctx, method, raw)
	if err != nil {
		return nil, authorizationError(err)
	}
	result, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if method == "list" {
		return result, nil
	}
	// IDs, offered options and intentional browser URLs are protocol data,
	// not diagnostics. Opaque records can contain ordinary account names and
	// scope strings; replacing those across this response breaks the next
	// request. The Go surface never returns answers or credential records.
	var view map[string]json.RawMessage
	if err := json.Unmarshal(result, &view); err != nil {
		return nil, err
	}
	if raw, ok := view["error"]; ok {
		var message string
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, err
		}
		view["error"], _ = json.Marshal(m.redact(message))
	}
	return json.Marshal(view)
}

func (m *Manager) authorizationHTTP(w http.ResponseWriter, r *http.Request, respond func(int, any), decode func(any) error) {
	suffix := strings.TrimPrefix(r.URL.Path, plugin.InternalPath+"/authorization")
	method := ""
	var input any = struct{}{}
	status := http.StatusOK
	switch {
	case suffix == "" && r.Method == http.MethodGet:
		method = "list"
	case suffix == "" && r.Method == http.MethodPost:
		var start plugin.AuthorizationStart
		if err := decode(&start); err != nil {
			respond(400, map[string]string{"error": "invalid_authorization_request"})
			return
		}
		method, input, status = "start", start, http.StatusAccepted
	case suffix == "/disconnect" && r.Method == http.MethodPost:
		var key struct {
			Key string `json:"key"`
		}
		if err := decode(&key); err != nil {
			respond(400, map[string]string{"error": "invalid_authorization_request"})
			return
		}
		method, input = "disconnect", key
	case suffix == "/callback" && r.Method == http.MethodPost:
		var callback struct {
			Token string              `json:"token"`
			Query map[string][]string `json:"query"`
		}
		if err := decode(&callback); err != nil {
			respond(400, map[string]string{"error": "invalid_authorization_callback"})
			return
		}
		method, input = "callback", callback
	case suffix == "" && r.Method == http.MethodDelete:
		var inputKey struct {
			Key string `json:"key"`
		}
		if err := decode(&inputKey); err != nil {
			respond(400, map[string]string{"error": "invalid_authorization_request"})
			return
		}
		method, input = "cancelKey", inputKey
	default:
		id, action, _ := strings.Cut(strings.TrimPrefix(suffix, "/"), "/")
		if _, err := uuid.Parse(id); err != nil {
			http.NotFound(w, r)
			return
		}
		switch {
		case action == "" && r.Method == http.MethodGet:
			method, input = "get", map[string]string{"id": id}
		case action == "" && r.Method == http.MethodDelete:
			method, input = "cancel", map[string]string{"id": id}
		case action == "answer" && r.Method == http.MethodPost:
			var answer plugin.AuthorizationAnswer
			if err := decode(&answer); err != nil {
				respond(400, map[string]string{"error": "invalid_authorization_answer"})
				return
			}
			method = "answer"
			input = struct {
				ID string `json:"id"`
				plugin.AuthorizationAnswer
			}{id, answer}
		default:
			http.NotFound(w, r)
			return
		}
	}
	ctx := r.Context()
	if base := r.Header.Get(plugin.AuthorizationCallbackHeader); base != "" {
		u, err := url.Parse(base)
		if err != nil || u.Path != plugin.AuthorizationCallbackPath+url.PathEscape(m.config.Runtime.UserID)+"/" {
			respond(400, map[string]string{"error": "invalid_authorization_callback"})
			return
		}
		ctx = authorization.WithCallbackBase(ctx, base)
	}
	result, err := m.Authorization(ctx, method, input)
	if err != nil {
		code := "authorization_failed"
		status = http.StatusUnprocessableEntity
		var detail *plugin.Error
		if errors.As(err, &detail) {
			code = detail.Code
			switch code {
			case "RUNTIME_UNAVAILABLE":
				status = http.StatusServiceUnavailable
			case "ALREADY_IN_FLIGHT", "AUTHORIZATION_BUSY":
				status = http.StatusConflict
			case "NO_FLOW", "AUTHORIZATION_NOT_FOUND", "PROMPT_NOT_FOUND":
				status = http.StatusNotFound
			}
		}
		respond(status, map[string]string{"error": code, "message": m.redact(err.Error())})
		return
	}
	respond(status, result)
}

func authorizationError(err error) error {
	var detail *authorization.Error
	if errors.As(err, &detail) {
		return &plugin.Error{Code: detail.Code, Message: detail.Message, Name: "AuthorizationError"}
	}
	return err
}
