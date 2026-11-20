package mcpclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	mcpconfig "go-bot/mcp"
)

func validateOAuthCallback(redirect string) error {
	if !validOAuthURL(redirect) {
		return fail("MCP_INVALID_CALLBACK", "OAuth callback must use HTTPS or loopback HTTP")
	}
	u, _ := url.Parse(redirect)
	account := strings.TrimPrefix(u.Path, mcpconfig.OAuthCallbackPath)
	if !strings.HasPrefix(u.Path, mcpconfig.OAuthCallbackPath) || account == "" || strings.Contains(account, "/") || u.RawQuery != "" || u.ForceQuery {
		return fail("MCP_INVALID_CALLBACK", "Invalid Controller OAuth callback path")
	}
	return nil
}

// ControllerOAuth belongs inside Host authentication. Only the Controller
// supplies this verified routing metadata, never a plugin or a model argument.
// Remembering the actual route lets a later background plugin action start a
// flow without guessing a public address or persisting deployment metadata.
func (m *Manager) ControllerOAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirect := r.Header.Get(mcpconfig.OAuthCallbackHeader); redirect != "" {
			if err := validateOAuthCallback(redirect); err != nil {
				http.Error(w, "Invalid Controller OAuth callback", http.StatusBadRequest)
				return
			}
			m.mu.Lock()
			if !m.closed {
				m.oauthCallback = redirect
			}
			m.mu.Unlock()
		}
		next.ServeHTTP(w, r)
	})
}

// StartAuthorization mirrors the plugin start contract: admission returns the
// current attempt immediately; URL preparation and completion publish status.
// The browser-facing HTTP Authorize operation additionally waits for its URL.
func (m *Manager) StartAuthorization(ctx context.Context, name string) (mcpconfig.OAuthAttemptView, error) {
	if err := ctx.Err(); err != nil {
		return mcpconfig.OAuthAttemptView{}, err
	}
	if err := m.ctx.Err(); err != nil {
		return mcpconfig.OAuthAttemptView{}, err
	}
	m.mu.RLock()
	redirect := m.oauthCallback
	if entry := m.entries[name]; entry != nil && entry.cleanupFailure != nil {
		m.mu.RUnlock()
		return mcpconfig.OAuthAttemptView{}, fail("MCP_CLEANUP_INCOMPLETE", "MCP authorization cleanup failed; restart the Host before authorizing again")
	}
	if previous := m.authorizations[name]; previous != nil {
		switch previous.view.Phase {
		case "starting", "waiting-browser", "exchanging", "committing":
			view := previous.view.AttemptView()
			m.mu.RUnlock()
			return *view, nil
		}
	}
	m.mu.RUnlock()
	if redirect == "" {
		return mcpconfig.OAuthAttemptView{}, fail("MCP_OAUTH_CALLBACK_UNAVAILABLE", "Starting MCP OAuth requires a verified Controller callback; connect through HTTPS or loopback HTTP first")
	}
	attempt, _, err := m.beginAuthorization(ctx, name, redirect)
	if err != nil {
		return mcpconfig.OAuthAttemptView{}, err
	}
	m.mu.RLock()
	view := attempt.view.AttemptView()
	m.mu.RUnlock()
	return *view, nil
}
