package controller

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	mcpconfig "go-bot/mcp"
)

func (m *Manager) mcpView(ctx context.Context, account Account) mcpconfig.View {
	status := mcpconfig.Status{Servers: []mcpconfig.ServerStatus{}}
	if account.DesiredState == running && account.HostEndpoint != "" {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := m.requestExtension(ctx, account, http.MethodGet, mcpconfig.InternalPath, nil, &status); err != nil {
			status.Error = "host_mcp_unavailable"
		}
	} else {
		status.Error = "host_not_running"
	}
	return mcpconfig.View{Status: status}
}
func (m *Manager) mcpHTTP(w http.ResponseWriter, r *http.Request, id string) {
	account, err := m.Store.Get(r.Context(), id)
	if errors.Is(err, ErrAccountNotFound) {
		httpError(w, 404, "account not found")
		return
	}
	if err != nil {
		httpError(w, 503, "mcp_settings_unavailable")
		return
	}
	runtimeSuffix, runtimeRequest := strings.CutPrefix(r.URL.EscapedPath(), mcpconfig.RuntimePath)
	if runtimeRequest && runtimeSuffix != "" {
		name, action, found := strings.Cut(strings.TrimPrefix(runtimeSuffix, "/servers/"), "/")
		postAction := r.Method == http.MethodPost && (action == "restart" || action == "probe" || action == "authorize")
		deleteAction := r.Method == http.MethodDelete && (action == "authorization" || action == "grant")
		if !strings.HasPrefix(runtimeSuffix, "/servers/") || !found || (!postAction && !deleteAction) {
			http.NotFound(w, r)
			return
		}
		if !mcpconfig.ValidServerName(name) {
			httpError(w, 400, "invalid_mcp_server")
			return
		}
		if account.DesiredState != running || account.HostEndpoint == "" {
			httpError(w, 409, "host_not_running")
			return
		}
		if action == "authorize" {
			m.startMCPOAuthHTTP(w, r, id, name, account)
			return
		}
		if deleteAction {
			target := mcpconfig.InternalPath + "/servers/" + name + "/" + action
			if action == "authorization" {
				attemptID := r.URL.Query().Get("id")
				if len(attemptID) > 128 {
					httpError(w, 400, "invalid_authorization_id")
					return
				}
				if attemptID != "" {
					target += "?id=" + url.QueryEscape(attemptID)
				}
			}
			if err = m.requestExtension(r.Context(), account, http.MethodDelete, target, nil, nil); err != nil {
				writeJSON(w, 502, map[string]string{"error": "mcp_authorization_failed", "message": err.Error()})
				return
			}
			writeJSON(w, 200, m.mcpView(r.Context(), account))
			return
		}
		if action == "probe" {
			var result mcpconfig.ServerStatus
			if err = m.requestExtension(r.Context(), account, http.MethodPost, mcpconfig.InternalPath+"/servers/"+name+"/probe", nil, &result); err != nil {
				httpError(w, 502, "host_mcp_probe_failed")
				return
			}
			writeJSON(w, 200, result)
			return
		}
		if err = m.requestExtension(r.Context(), account, http.MethodPost, mcpconfig.InternalPath+"/servers/"+name+"/restart", nil, nil); err != nil {
			httpError(w, 502, "host_mcp_restart_failed")
			return
		}
		writeJSON(w, 200, m.mcpView(r.Context(), account))
		return
	}
	if r.URL.Path == mcpconfig.RuntimePath && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, m.mcpView(r.Context(), account))
		return
	}
	http.NotFound(w, r)
}
