package controller

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	mcpconfig "go-bot/mcp"
	"go-bot/plugin"
)

// Derive the second callback from the already validated public Controller
// origin and account. It is routing metadata, not another authorization flow.
func pluginCallbackBase(redirect string) string {
	u, err := url.Parse(redirect)
	if err != nil || !strings.HasPrefix(u.Path, mcpconfig.OAuthCallbackPath) {
		return ""
	}
	account := strings.TrimPrefix(u.Path, mcpconfig.OAuthCallbackPath)
	if !userName.MatchString(account) {
		return ""
	}
	u.Path = plugin.AuthorizationCallbackPath + account + "/"
	return u.String()
}

func (m *Manager) pluginAuthorizationCallbackHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOAuthCallbackPage(w, r, http.StatusMethodNotAllowed, "")
		return
	}
	accountID, token, found := strings.Cut(strings.TrimPrefix(r.URL.Path, plugin.AuthorizationCallbackPath), "/")
	if !found || !userName.MatchString(accountID) || uuid.Validate(token) != nil || len(r.URL.RawQuery) > 24<<10 {
		writeOAuthCallbackPage(w, r, http.StatusBadRequest, "")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["state"]) != 1 || query.Get("state") == "" {
		writeOAuthCallbackPage(w, r, http.StatusBadRequest, "")
		return
	}
	account, err := m.Store.Get(r.Context(), accountID)
	if err != nil || account.DesiredState != running || account.HostEndpoint == "" {
		writeOAuthCallbackPage(w, r, http.StatusConflict, "")
		return
	}
	var receipt struct {
		Received bool `json:"received"`
	}
	if err := m.requestPlugins(r.Context(), account, http.MethodPost, "/authorization/callback", map[string]any{"token": token, "query": query}, &receipt); err != nil || !receipt.Received {
		writeOAuthCallbackPage(w, r, http.StatusBadRequest, "")
		return
	}
	// This says only that Go received the callback. The plugin still validates
	// the provider response and exchanges tokens before it can commit a grant.
	writeOAuthCallbackPage(w, r, http.StatusOK, "received")
}
