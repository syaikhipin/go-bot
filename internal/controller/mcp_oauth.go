package controller

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"go-bot/internal/config"
	mcpconfig "go-bot/mcp"
)

type oauthCallbackContextKey struct{}

func withOAuthCallback(ctx context.Context, redirect string) context.Context {
	return context.WithValue(ctx, oauthCallbackContextKey{}, redirect)
}

func oauthCallbackFromContext(ctx context.Context) string {
	value, _ := ctx.Value(oauthCallbackContextKey{}).(string)
	return value
}

// The client supplies only its visible origin, never a free-form redirect.
// Host is the actual request Host; forwarded host/proto headers are not trusted
// to select an OAuth destination. A proxy should preserve the public Host.
func mcpOAuthRedirect(r *http.Request, origin, user string) (string, error) {
	if origin == "" {
		origin = r.Header.Get("Origin")
	}
	if origin == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		origin = scheme + "://" + r.Host
	}
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || len(origin) > 4096 {
		return "", errors.New("invalid_controller_origin")
	}
	defaultPort := ":80"
	if u.Scheme == "https" {
		defaultPort = ":443"
	}
	if !strings.EqualFold(strings.TrimSuffix(u.Host, defaultPort), strings.TrimSuffix(r.Host, defaultPort)) {
		return "", errors.New("controller_origin_must_match_request_host")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !config.IsLoopbackHost(u.Hostname())) {
		return "", errors.New("oauth_requires_https_or_loopback_http")
	}
	return u.Scheme + "://" + u.Host + mcpconfig.OAuthCallbackPath + url.PathEscape(user), nil
}

func (m *Manager) startMCPOAuthHTTP(w http.ResponseWriter, r *http.Request, id, name string, account Account) {
	var input struct {
		Origin string `json:"origin"`
	}
	if decodeJSON(http.MaxBytesReader(w, r.Body, 16<<10), &input) != nil {
		httpError(w, 400, "invalid_mcp_authorization_request")
		return
	}
	redirect, err := mcpOAuthRedirect(r, input.Origin, id)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	var authorization mcpconfig.OAuthAuthorization
	if err := m.requestExtension(withOAuthCallback(r.Context(), redirect), account, http.MethodPost, mcpconfig.InternalPath+"/servers/"+name+"/authorize", map[string]string{"redirectUrl": redirect}, &authorization); err != nil {
		writeJSON(w, 502, map[string]string{"error": "mcp_authorization_failed", "message": err.Error()})
		return
	}
	writeJSON(w, 200, authorization)
}

func writeOAuthCallbackPage(w http.ResponseWriter, r *http.Request, status int, state string) {
	title, text := "Authorization could not be completed", "Return to Go Bot and start authorization again."
	if state == "received" {
		title, text = "Authorization received", "You can close this page and check the connection in Go Bot."
	}
	if state == "authorized" {
		title, text = "Authorization completed", "You can close this page and return to Go Bot."
	}
	if state == "pending" {
		title, text = "Additional authorization needed", "Return to Go Bot to continue the provider authorization."
	}
	lang := "en"
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Accept-Language")), "zh") {
		lang = "zh-CN"
		title, text = "未能完成授权", "请返回 Go Bot，重新发起授权。"
		if state == "received" {
			title, text = "已收到授权", "可以关闭此页面，返回 Go Bot 查看连接结果。"
		}
		if state == "authorized" {
			title, text = "授权已完成", "可以关闭此页面，返回 Go Bot。"
		}
		if state == "pending" {
			title, text = "需要补充授权", "请返回 Go Bot，继续服务商要求的授权。"
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.WriteHeader(status)
	// All interpolated strings are fixed application copy, never provider or
	// callback input. Authorization codes and state do not enter the page.
	_, _ = w.Write([]byte(`<!doctype html><html lang="` + lang + `"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + title + `</title><style>body{font:16px system-ui,sans-serif;color:#263145;background:#f6f8fc;max-width:520px;margin:16vh auto;padding:32px}h1{font-size:24px;letter-spacing:-.4px}p{line-height:1.7;color:#637087}</style><h1>` + title + `</h1><p>` + text + `</p></html>`))
}

func (m *Manager) mcpOAuthCallbackHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOAuthCallbackPage(w, r, 405, "")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, mcpconfig.OAuthCallbackPath)
	if !userName.MatchString(id) {
		writeOAuthCallbackPage(w, r, 400, "")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(r.URL.RawQuery) > 24<<10 {
		writeOAuthCallbackPage(w, r, 400, "")
		return
	}
	for _, name := range []string{"state", "code", "iss", "error"} {
		if len(query[name]) > 1 {
			writeOAuthCallbackPage(w, r, 400, "")
			return
		}
	}
	if query.Get("state") == "" || query.Has("code") == query.Has("error") || (query.Has("code") && query.Get("code") == "") || (query.Has("error") && query.Get("error") == "") {
		writeOAuthCallbackPage(w, r, 400, "")
		return
	}
	account, err := m.Store.Get(r.Context(), id)
	if err != nil {
		writeOAuthCallbackPage(w, r, 404, "")
		return
	}
	if account.DesiredState != running || account.HostEndpoint == "" {
		writeOAuthCallbackPage(w, r, 409, "")
		return
	}
	var view mcpconfig.OAuthCallbackReceipt
	err = m.requestExtension(r.Context(), account, http.MethodPost, mcpconfig.InternalPath+"/oauth/callback", mcpconfig.OAuthCallback{State: query.Get("state"), Code: query.Get("code"), Issuer: query.Get("iss"), Error: query.Get("error")}, &view)
	if err != nil || (view.State != "received" && view.State != "rejected") {
		writeOAuthCallbackPage(w, r, 400, "")
		return
	}
	writeOAuthCallbackPage(w, r, 200, view.State)
}
