package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	mcpconfig "go-bot/mcp"
)

type oauthTestService struct {
	resource, provider      *httptest.Server
	server                  *sdk.Server
	mu                      sync.Mutex
	codes                   map[string]string
	tokens                  map[string]bool
	sequence                int
	lastAccess, lastRefresh string
	refreshes               atomic.Int32
	requests                atomic.Int32
	registrations           atomic.Int32
	denyRefresh             atomic.Bool
	denyClient              atomic.Bool
	dynamicConfidential     atomic.Bool
	denyResource            atomic.Bool
	omitIssuerAdvertisement atomic.Bool
}

func oauthServiceFixture(t *testing.T) *oauthTestService {
	t.Helper()
	f := &oauthTestService{codes: map[string]string{}, tokens: map[string]bool{}}
	f.provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(value any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(value)
		}
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			write(map[string]any{"issuer": f.provider.URL, "authorization_endpoint": f.provider.URL + "/authorize", "token_endpoint": f.provider.URL + "/token", "registration_endpoint": f.provider.URL + "/register", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none", "client_secret_post"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{"read", "offline_access"}, "authorization_response_iss_parameter_supported": !f.omitIssuerAdvertisement.Load()})
		case "/register":
			f.registrations.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			registration := map[string]any{"client_id": "dynamic-client", "token_endpoint_auth_method": "none"}
			if f.dynamicConfidential.Load() {
				registration["token_endpoint_auth_method"] = "client_secret_post"
				registration["client_secret"] = "private-dynamic-client-secret"
				registration["client_secret_expires_at"] = 0
			}
			write(registration)
		case "/authorize":
			query := r.URL.Query()
			if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
				http.Error(w, "PKCE required", 400)
				return
			}
			f.mu.Lock()
			f.sequence++
			code := fmt.Sprint("code-", f.sequence)
			f.codes[code] = query.Get("code_challenge")
			f.mu.Unlock()
			callback, err := url.Parse(query.Get("redirect_uri"))
			if err != nil {
				http.Error(w, "bad redirect", 400)
				return
			}
			values := callback.Query()
			values.Set("code", code)
			values.Set("state", query.Get("state"))
			values.Set("iss", f.provider.URL)
			callback.RawQuery = values.Encode()
			http.Redirect(w, r, callback.String(), http.StatusFound)
		case "/token":
			if f.denyClient.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				write(map[string]string{"error": "invalid_client", "error_description": "rejected private-dynamic-client-secret"})
				return
			}
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad token form", 400)
				return
			}
			if r.Form.Get("client_id") == "confidential-client" && r.Form.Get("client_secret") != "private-user-client-secret" {
				http.Error(w, "invalid confidential client", 400)
				return
			}
			if r.Form.Get("client_id") == "dynamic-client" && f.dynamicConfidential.Load() && r.Form.Get("client_secret") != "private-dynamic-client-secret" {
				http.Error(w, "dynamic registration auth method or secret was lost", 400)
				return
			}
			if r.Form.Get("grant_type") == "authorization_code" {
				f.mu.Lock()
				challenge, found := f.codes[r.Form.Get("code")]
				delete(f.codes, r.Form.Get("code"))
				f.mu.Unlock()
				digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
				if !found || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge || r.Form.Get("resource") != f.resource.URL+"/mcp" {
					http.Error(w, "bad code, verifier or resource", 400)
					return
				}
			} else if r.Form.Get("grant_type") == "refresh_token" {
				f.refreshes.Add(1)
				if r.Form.Get("resource") != f.resource.URL+"/mcp" {
					http.Error(w, "refresh lost its authorized resource", 400)
					return
				}
				if f.denyRefresh.Load() {
					w.WriteHeader(400)
					write(map[string]string{"error": "invalid_grant"})
					return
				}
				if !strings.HasPrefix(r.Form.Get("refresh_token"), "refresh-") {
					http.Error(w, "bad refresh", 400)
					return
				}
			} else {
				http.Error(w, "bad grant type", 400)
				return
			}
			f.mu.Lock()
			f.sequence++
			access := fmt.Sprint("access-", f.sequence)
			refresh := fmt.Sprint("refresh-", f.sequence)
			f.tokens[access] = true
			f.lastAccess, f.lastRefresh = access, refresh
			f.mu.Unlock()
			write(map[string]any{"access_token": access, "refresh_token": refresh, "expires_in": 3600, "token_type": "Bearer", "scope": "read offline_access"})
		default:
			http.NotFound(w, r)
		}
	}))
	server := testServer()
	f.server = server
	mcpHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	f.resource = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource/mcp" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": f.resource.URL + "/mcp", "authorization_servers": []string{f.provider.URL}, "scopes_supported": []string{"read"}})
			return
		}
		f.requests.Add(1)
		f.mu.Lock()
		authorized := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		f.mu.Unlock()
		if !authorized {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.resource.URL+`/.well-known/oauth-protected-resource/mcp"`)
			http.Error(w, "authorization required", 401)
			return
		}
		if f.denyResource.Load() {
			http.Error(w, "MCP temporarily unavailable after authorization", http.StatusServiceUnavailable)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(f.resource.Close)
	t.Cleanup(f.provider.Close)
	return f
}

func TestOAuthDynamicAndConfidentialClientsUseTheSameExplicitFlow(t *testing.T) {
	for _, client := range []string{"", "confidential-client"} {
		t.Run("client-"+client, func(t *testing.T) {
			f := oauthServiceFixture(t)
			manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
			defer manager.Close()
			settings := f.settings(client)
			if client != "" {
				server := settings.Servers["oauth"]
				server.OAuth.ClientSecretRef = "oauth/client-secret"
				settings.Servers["oauth"] = server
				settings.Credentials = map[string]string{"oauth/client-secret": "private-user-client-secret"}
			}
			if _, err := manager.Apply(t.Context(), settings); err != nil {
				t.Fatal(err)
			}
			if status := manager.Status().Servers[0]; !status.OAuthEnabled || status.Authorization != nil {
				t.Fatalf("OAuth capability must be discoverable before starting a flow: %+v", status)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view)); err != nil {
				t.Fatal(err)
			}
			if manager.Status().Servers[0].State != "connected" {
				t.Fatal("client did not connect")
			}
		})
	}
}

func TestOAuthRevokedLiveTokenRefreshesAndRejectedRefreshWaitsForExplicitAuthorization(t *testing.T) {
	f := oauthServiceFixture(t)
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
	defer manager.Close()
	if _, err := manager.Apply(t.Context(), f.settings("test-client")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view)); err != nil {
		t.Fatal(err)
	}
	scope := manager.NewScope()
	defer scope.Close()
	tool := findTool(t, scope, PublicToolName("oauth", "one"))
	if tool == nil {
		t.Fatal("missing authorized tool")
	}
	f.mu.Lock()
	f.tokens = map[string]bool{}
	f.mu.Unlock()
	if _, err := tool.Execute(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatal("Resource rejection did not refresh and retry", err)
	}
	if f.refreshes.Load() != 1 || manager.Status().Servers[0].State != "connected" {
		t.Fatal("Resource rejection did not recover with one refresh")
	}
	f.denyRefresh.Store(true)
	f.mu.Lock()
	f.tokens = map[string]bool{}
	f.mu.Unlock()
	if _, err := tool.Execute(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("Both revoked access and rejected refresh still executed a remote tool")
	}
	for {
		changes, _ := manager.Changes()
		if manager.Status().Servers[0].State == "waiting-authorization" {
			break
		}
		select {
		case <-changes:
		case <-ctx.Done():
			t.Fatal("revoked token did not enter waiting-authorization")
		}
	}
	manager.mu.RLock()
	entry := manager.entries["oauth"]
	attempts := len(manager.authorizations)
	manager.mu.RUnlock()
	select {
	case <-entry.done:
	case <-ctx.Done():
		t.Fatal("authorization failure continued reconnecting")
	}
	if attempts != 0 {
		t.Fatal("background tool failure implicitly opened an OAuth authorization")
	}
	if f.refreshes.Load() != 2 {
		t.Fatalf("Rejected refresh was repeated: requests=%d", f.refreshes.Load())
	}
}

func TestOAuthIssuerMismatchDoesNotPersistGrant(t *testing.T) {
	f := oauthServiceFixture(t)
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
	defer manager.Close()
	if _, err := manager.Apply(t.Context(), f.settings("test-client")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if err != nil {
		t.Fatal(err)
	}
	callback := visitOAuthAuthorization(t, view)
	callback.Issuer = "https://other-issuer.example"
	if result, err := manager.CompleteAuthorization(ctx, callback); err == nil {
		t.Fatalf("mismatched OAuth issuer was accepted: state=%s url=%t", result.State, result.URL != "")
	}
	if _, found, err := manager.oauthStore.get("oauth", manager.oauthConfigBinding("oauth", "", f.settings("test-client").Servers["oauth"])); err != nil || found {
		t.Fatal("issuer mismatch persisted a grant", err)
	}
}

func (f *oauthTestService) settings(client string) mcpconfig.Settings {
	return mcpconfig.Settings{Servers: map[string]mcpconfig.Server{"oauth": {Transport: "streamable-http", URL: f.resource.URL + "/mcp", OAuth: &mcpconfig.OAuth{ClientID: client}}}}
}

func visitOAuthAuthorization(t *testing.T, view mcpconfig.OAuthAuthorization) mcpconfig.OAuthCallback {
	t.Helper()
	if view.State != "pending" || view.URL == "" || view.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("not a real pending authorization: %+v", view)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 3 * time.Second}
	response, err := client.Get(view.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("authorize status=%d", response.StatusCode)
	}
	location, err := response.Location()
	if err != nil {
		t.Fatal(err)
	}
	query := location.Query()
	return mcpconfig.OAuthCallback{State: query.Get("state"), Code: query.Get("code"), Issuer: query.Get("iss")}
}

func TestOAuthExplicitPKCECallbackConnectsAndRestoresWithRefresh(t *testing.T) {
	f := oauthServiceFixture(t)
	directory := t.TempDir()
	manager := NewManager(t.Context(), ManagerOptions{CWD: directory})
	defer manager.Close()
	settings := f.settings("test-client")
	if status, err := manager.Apply(t.Context(), settings); err != nil || status.Servers[0].State != "waiting-authorization" {
		t.Fatalf("initial=%+v err=%v", status, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if err != nil {
		t.Fatal("start OAuth", err)
	}
	callback := visitOAuthAuthorization(t, view)
	completed, err := manager.CompleteAuthorization(ctx, callback)
	if err != nil || completed.State != "authorized" {
		t.Fatalf("callback=%+v err=%v", completed, err)
	}
	if status := manager.Status(); status.Servers[0].State != "connected" || status.Servers[0].ToolCount != 2 {
		t.Fatalf("authorized connection=%+v", status)
	}
	if _, err := manager.CompleteAuthorization(ctx, callback); err == nil {
		t.Fatal("one-time OAuth callback was replayed")
	}
	if result, err := manager.Probe(ctx, "oauth"); err != nil || result.State != "connected" {
		t.Fatalf("authorized probe=%+v err=%v", result, err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	store, _ := newOAuthGrantStore(filepath.Join(directory, ".dsh", ".credentials.yaml"))
	grant, found, err := store.get("oauth", manager.oauthConfigBinding("oauth", "", settings.Servers["oauth"]))
	if err != nil || !found {
		t.Fatal("OAuth grant was not persisted", err)
	}
	grant.Token.Expiry = time.Now().Add(-time.Hour)
	if err := store.put(ctx, "oauth", grant); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(t.Context(), ManagerOptions{CWD: directory})
	defer restored.Close()
	if status, err := restored.Apply(ctx, settings); err != nil || status.Servers[0].State != "connected" {
		t.Fatalf("restart=%+v err=%v", status, err)
	}
	if f.refreshes.Load() != 1 {
		t.Fatalf("refreshes=%d; want one", f.refreshes.Load())
	}
	public, _ := json.Marshal(restored.Status())
	if strings.Contains(string(public), "access-") || strings.Contains(string(public), "refresh-") {
		t.Fatal("public status leaked a token")
	}
	if status, err := restored.DisconnectAuthorization(ctx, "oauth"); err != nil || status.Servers[0].State != "waiting-authorization" {
		t.Fatalf("disconnect=%+v err=%v", status, err)
	}
	afterSignOut, err := newOAuthGrantStore(filepath.Join(directory, ".dsh", ".credentials.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := afterSignOut.get("oauth", grant.Binding); err != nil || found {
		t.Fatal("sign-out retained the local OAuth grant", err)
	}
}

func TestOAuthCancellationAndResourceChangeNeverForwardOldGrant(t *testing.T) {
	f := oauthServiceFixture(t)
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
	defer manager.Close()
	settings := f.settings("test-client")
	if _, err := manager.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if err != nil {
		t.Fatal(err)
	}
	stateURL, _ := url.Parse(view.URL)
	if _, err := manager.CancelAuthorization(ctx, "oauth"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompleteAuthorization(ctx, mcpconfig.OAuthCallback{State: stateURL.Query().Get("state"), Code: "late"}); err == nil {
		t.Fatal("cancelled flow accepted a callback")
	}
	view, err = manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view)); err != nil {
		t.Fatal(err)
	}
	before := f.requests.Load()
	changed := settings.Clone()
	server := changed.Servers["oauth"]
	server.URL = f.resource.URL + "/other-resource"
	changed.Servers["oauth"] = server
	if status, err := manager.Apply(ctx, changed); err != nil || status.Servers[0].State != "waiting-authorization" {
		t.Fatalf("changed resource=%+v err=%v", status, err)
	}
	if f.requests.Load() != before+1 { // Closing the old initialized session sends its DELETE.
		t.Fatalf("unexpected requests to the changed resource: before=%d after=%d", before, f.requests.Load())
	}
}

func TestOAuthRejectedRefreshWaitsForAuthorizationWithoutRetryOrBrowserFlow(t *testing.T) {
	f := oauthServiceFixture(t)
	directory := t.TempDir()
	manager := NewManager(t.Context(), ManagerOptions{CWD: directory})
	defer manager.Close()
	settings := f.settings("test-client")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := manager.Apply(ctx, settings); err != nil {
		t.Fatal(err)
	}
	view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	persisted, err := newOAuthGrantStore(filepath.Join(directory, ".dsh", ".credentials.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	grant, found, err := persisted.get("oauth", manager.oauthConfigBinding("oauth", "", settings.Servers["oauth"]))
	if err != nil || !found {
		t.Fatal("missing original grant", err)
	}
	grant.Token.Expiry = time.Now().Add(-time.Hour)
	if err := persisted.put(ctx, "oauth", grant); err != nil {
		t.Fatal(err)
	}
	f.denyRefresh.Store(true)
	restored := NewManager(t.Context(), ManagerOptions{CWD: directory})
	defer restored.Close()
	status, err := restored.Apply(ctx, settings)
	if err != nil || status.Servers[0].State != "waiting-authorization" || status.Servers[0].Authorization != nil {
		t.Fatalf("refresh rejection status=%+v err=%v", status, err)
	}
	restored.mu.RLock()
	entry := restored.entries["oauth"]
	attempts := len(restored.authorizations)
	restored.mu.RUnlock()
	select {
	case <-entry.done:
	case <-ctx.Done():
		t.Fatal("rejected refresh continued reconnecting")
	}
	if attempts != 0 || f.refreshes.Load() != 1 {
		t.Fatalf("rejected refresh started another flow: attempts=%d refreshes=%d", attempts, f.refreshes.Load())
	}
}

func TestOAuthDisabledServerCanAuthorizeWithoutPublishingTools(t *testing.T) {
	f := oauthServiceFixture(t)
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
	defer manager.Close()
	settings := f.settings("test-client")
	server := settings.Servers["oauth"]
	enabled := false
	server.Enabled = &enabled
	settings.Servers["oauth"] = server
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := manager.Apply(ctx, settings); err != nil {
		t.Fatal(err)
	}
	view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view))
	if err != nil || result.State != "authorized" {
		t.Fatalf("disabled authorization=%+v err=%v", result, err)
	}
	status := manager.Status().Servers[0]
	if status.State != "disabled" || status.ToolCount != 0 {
		t.Fatalf("authorization enabled a disabled server: %+v", status)
	}
	enabled = true
	if status, err := manager.Apply(ctx, settings); err != nil || status.Servers[0].State != "connected" {
		t.Fatalf("stored grant did not connect after enabling: %+v err=%v", status, err)
	}
}

func TestOAuthExpiredAttemptClearsURLAndRejectsLateCallback(t *testing.T) {
	f := oauthServiceFixture(t)
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
	defer manager.Close()
	if _, err := manager.Apply(t.Context(), f.settings("test-client")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(manager.ctx, time.Now().Add(-time.Second))
	defer cancel()
	manager.mu.Lock()
	entry := manager.entries["oauth"]
	attempt := &oauthAttempt{entry: entry, ctx: ctx, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}, 1), answer: make(chan oauthCallbackResult, 1), state: "expired-state", view: mcpconfig.OAuthAuthorization{Name: "oauth", State: "pending", URL: f.provider.URL + "/authorize", ExpiresAt: time.Now().Add(-time.Second).UnixMilli()}}
	manager.authorizations["oauth"] = attempt
	manager.mu.Unlock()
	manager.runAuthorization(attempt, "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if view := manager.Status().Servers[0].Authorization; view == nil || view.State != "expired" || view.URL != "" {
		t.Fatalf("expired flow was still actionable: %+v", view)
	}
	if _, err := manager.CompleteAuthorization(t.Context(), mcpconfig.OAuthCallback{State: "expired-state", Code: "late"}); err == nil {
		t.Fatal("expired state accepted a callback")
	}
}

func TestOAuthRefreshedTokensAreRedactedFromRemoteToolErrors(t *testing.T) {
	f := oauthServiceFixture(t)
	f.server.AddTool(&sdk.Tool{Name: "one", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "provider echoed " + f.lastAccess + " " + f.lastRefresh}}}, nil
	})
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
	defer manager.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	settings := f.settings("test-client")
	if _, err := manager.Apply(ctx, settings); err != nil {
		t.Fatal(err)
	}
	view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view)); err != nil {
		t.Fatal(err)
	}
	grant, found, err := manager.oauthStore.get("oauth", manager.oauthConfigBinding("oauth", "", settings.Servers["oauth"]))
	if err != nil || !found {
		t.Fatal("missing original grant", err)
	}
	grant.Token.Expiry = time.Now().Add(-time.Hour)
	if err := manager.oauthStore.put(ctx, "oauth", grant); err != nil {
		t.Fatal(err)
	}
	if status, err := manager.Restart(ctx, "oauth"); err != nil || status.Servers[0].State != "connected" {
		t.Fatalf("refreshed server=%+v err=%v", status, err)
	}
	f.mu.Lock()
	access, refresh := f.lastAccess, f.lastRefresh
	f.mu.Unlock()
	if access == grant.Token.AccessToken || f.refreshes.Load() != 1 {
		t.Fatal("fixture did not rotate the original tokens")
	}
	scope := manager.NewScope()
	defer scope.Close()
	tool := findTool(t, scope, PublicToolName("oauth", "one"))
	if tool == nil {
		t.Fatal("missing refreshed tool")
	}
	result, err := tool.(*Tool).ExecuteResult(ctx, json.RawMessage(`{}`))
	data, marshalErr := json.Marshal(result)
	if err != nil || marshalErr != nil || !result.IsError || !strings.Contains(string(data), "[redacted]") || strings.Contains(string(data), access) || strings.Contains(string(data), refresh) {
		t.Fatal("refreshed OAuth token escaped into the tool result or canonical metadata", err, marshalErr)
	}
}
