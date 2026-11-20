package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	mcpconfig "go-bot/mcp"
)

func waitRuntimeStatus(t *testing.T, manager *Manager, state string) mcpconfig.ManagedServerStatus {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		changed, _ := manager.Changes()
		for _, row := range manager.ListViews() {
			if row.Status.State == state {
				return row.Status
			}
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("did not reach %s: %+v", state, manager.ListViews())
		}
	}
}

func TestRuntimeStatusUsesRealConnectAndReconnectTimings(t *testing.T) {
	var unavailable atomic.Bool
	unavailable.Store(true)
	server := testServer()
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			http.Error(w, "temporary outage", 503)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer remote.Close()
	manager := NewManager(t.Context(), ManagerOptions{})
	defer manager.Close()
	before := time.Now().UnixMilli()
	if _, err := manager.Apply(t.Context(), mcpconfig.Settings{Servers: map[string]mcpconfig.Server{"fixture": {Transport: "streamable-http", URL: remote.URL}}}); err == nil {
		t.Fatal("first failed attempt should report its failure")
	}
	retrying := waitRuntimeStatus(t, manager, "reconnecting")
	if retrying.Attempt < 1 || retrying.MaxAttempts != 10 || retrying.NextRetryAtMS < before+450 || retrying.LastError == "" {
		t.Fatalf("retry facts missing: %+v", retrying)
	}
	unavailable.Store(false)
	connected := waitRuntimeStatus(t, manager, "connected")
	if connected.ToolCount != 2 || connected.ConnectedAt < before || connected.ConnectedAt > time.Now().UnixMilli() {
		t.Fatalf("connect facts missing: %+v", connected)
	}
	server.AddTool(&sdk.Tool{Name: "third", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		changed, _ := manager.Changes()
		status := manager.ListViews()[0].Status
		if status.ToolCount == 3 {
			if status.ConnectedAt != connected.ConnectedAt {
				t.Fatal("catalog sync reset the connection timestamp")
			}
			break
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatal("tools notification was not reflected")
		}
	}
	data, err := json.Marshal(manager.ListViews()[0])
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	status := row["status"].(map[string]any)
	if len(status) != 3 || status["connectedAt"] == nil || status["toolCount"] != float64(3) {
		t.Fatalf("status is not the connected union: %s", data)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if status := manager.ListViews()[0].Status; status.State != "closed" || status.Result == nil || !status.Result.CloseConfirmed {
		t.Fatalf("closed manager advertises a new connection: %+v", status)
	}
}

func TestRuntimeStatusRetainsFailedGenerationCleanup(t *testing.T) {
	client, remote := sdk.NewInMemoryTransports()
	server := testServer()
	session, err := server.Connect(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	manager := NewManager(t.Context(), ManagerOptions{Connection: ConnectionOptions{Transport: probeCloseErrorTransport{base: client}}})
	defer manager.Close()
	if _, err := manager.Apply(t.Context(), mcpconfig.Settings{Servers: map[string]mcpconfig.Server{"fixture": {Transport: "stdio", Command: "not-spawned"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Restart(t.Context(), "fixture"); err == nil {
		t.Fatal("failed cleanup allowed a replacement")
	}
	status := manager.ListViews()[0].Status
	if status.State != "failed" || status.Reason != "cleanup" || status.Result == nil || !status.Result.CloseConfirmed || status.Result.CleanupError == "" {
		t.Fatalf("cleanup facts missing: %+v", status)
	}
	if _, err := manager.Restart(t.Context(), "fixture"); err == nil {
		t.Fatal("retained cleanup failure was forgotten")
	}
}

func TestRuntimeAuthorizationReasonsComeFromCredentialAndProviderFailures(t *testing.T) {
	f := oauthServiceFixture(t)
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
	defer manager.Close()
	settings := f.settings("test-client")
	if _, err := manager.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if status := manager.ListViews()[0].Status; status.State != "waiting-authorization" || status.Reason != "missing" || status.LastError == "" {
		t.Fatalf("missing grant reason: %+v", status)
	}
	grant := fixtureOAuthGrant()
	grant.Binding = "wrong-binding"
	if err := manager.oauthStore.put(t.Context(), "oauth", grant); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Restart(t.Context(), "oauth"); err != nil {
		t.Fatal(err)
	}
	if status := manager.ListViews()[0].Status; status.Reason != "binding" {
		t.Fatalf("binding mismatch reason: %+v", status)
	}
	manager.mu.RLock()
	grant.Binding = manager.entries["oauth"].oauthBinding
	manager.mu.RUnlock()
	grant.ServerURL = settings.Servers["oauth"].URL
	configuredClientID := settings.Servers["oauth"].OAuth.ClientID
	grant.ConfiguredClientID = &configuredClientID
	grant.Token.Expiry = time.Now().Add(-time.Hour)
	grant.Token.RefreshToken = ""
	if err := manager.oauthStore.put(t.Context(), "oauth", grant); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Restart(t.Context(), "oauth"); err != nil {
		t.Fatal(err)
	}
	if status := manager.ListViews()[0].Status; status.Reason != "expired" {
		t.Fatalf("expired grant reason: %+v", status)
	}
	response := httptest.NewRecorder()
	response.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
	response.WriteHeader(http.StatusForbidden)
	err := (&storedOAuthHandler{}).Authorize(t.Context(), nil, response.Result())
	var detail *Error
	if !errors.As(err, &detail) || detail.AuthorizationReason != "scope" {
		t.Fatalf("provider scope reason lost: %v", err)
	}
}

func TestAuthorizationAttemptIdentityAndSelectedCancellation(t *testing.T) {
	f := oauthServiceFixture(t)
	manager := NewManager(t.Context(), ManagerOptions{OAuthCredentialsPath: filepath.Join(t.TempDir(), "credentials.json")})
	defer manager.Close()
	if _, err := manager.Apply(t.Context(), f.settings("test-client")); err != nil {
		t.Fatal(err)
	}
	redirect := "http://127.0.0.1:9090" + mcpconfig.OAuthCallbackPath + "alice"
	first, err := manager.Authorize(t.Context(), "oauth", redirect)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.Phase != "waiting-browser" {
		t.Fatalf("attempt identity missing: %+v", first)
	}
	if err := manager.CancelAuthorizationAttempt(t.Context(), "oauth", "stale-id"); err != nil {
		t.Fatal(err)
	}
	if current := manager.ListViews()[0].Authorization; current == nil || current.ID != first.ID || current.Phase != "waiting-browser" || current.AuthorizationURL == "" {
		t.Fatalf("stale cancellation touched live attempt: %+v", current)
	}
	if err := manager.CancelAuthorizationAttempt(t.Context(), "oauth", first.ID); err != nil {
		t.Fatal(err)
	}
	if current := manager.ListViews()[0].Authorization; current == nil || current.ID != first.ID || current.Phase != "cancelled" || current.AuthorizationURL != "" {
		t.Fatalf("selected cancellation was not retained: %+v", current)
	}
	second, err := manager.Authorize(t.Context(), "oauth", redirect)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("replacement reused the attempt identity")
	}
	if err := manager.CancelAuthorizationAttempt(t.Context(), "oauth", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CompleteAuthorization(t.Context(), visitOAuthAuthorization(t, second)); err != nil {
		t.Fatal(err)
	}
	if current := manager.ListViews()[0].Authorization; current == nil || current.ID != second.ID || current.Phase != "authorized" || current.AuthorizationURL != "" {
		t.Fatalf("completion lost attempt identity: %+v", current)
	}
}
