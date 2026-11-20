package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go-bot/internal/config"
	"go-bot/internal/httpapi"
	"go-bot/internal/mcpclient"
	mcpconfig "go-bot/mcp"
)

func TestMCPIsPluginOwnedAndHasNoAccountConfigurationAPI(t *testing.T) {
	remote := sdk.NewServer(&sdk.Implementation{Name: "controller-fixture", Version: "1"}, nil)
	remote.AddTool(&sdk.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	remoteHTTP := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return remote }, nil))
	defer remoteHTTP.Close()
	fleet := mcpclient.NewManager(t.Context(), mcpclient.ManagerOptions{})
	defer fleet.Close()
	// Same Go entry point used by a Cordis plugin contribution. The account
	// stores no parallel MCP intent or credential document.
	if err := fleet.Register(t.Context(), "knowledge", "knowledge-plugin:1", mcpconfig.Server{Transport: "streamable-http", URL: remoteHTTP.URL, Headers: map[string]string{"Authorization": "Bearer fixture-private"}}); err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(httpapi.Authenticate("private-host-token", fleet.Handler()))
	defer host.Close()
	store := &memoryStore{records: map[string]Account{"alice": {DesiredState: running, HostEndpoint: host.URL, HostToken: "private-host-token"}}}
	controller := New(config.Default(), nil, store)
	request := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set(UserHeader, "alice")
		w := httptest.NewRecorder()
		controller.Handler().ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/v1/settings/mcp", "/v1/settings/mcp/servers/knowledge", "/v1/settings/mcp/credentials/key"} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			if w := request(method, path); w.Code != http.StatusNotFound {
				t.Fatalf("removed configuration API %s %s = %d", method, path, w.Code)
			}
		}
	}
	for _, path := range []string{mcpconfig.RuntimePath, mcpconfig.RuntimePath + "/servers/knowledge/probe", mcpconfig.RuntimePath + "/servers/knowledge/restart"} {
		method := http.MethodPost
		if path == mcpconfig.RuntimePath {
			method = http.MethodGet
		}
		w := request(method, path)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "fixture-private") || strings.Contains(w.Body.String(), "private-host-token") || strings.Contains(w.Body.String(), `"settings"`) {
			t.Fatalf("runtime response %s = %d %s", path, w.Code, w.Body)
		}
	}
	if status := fleet.Status(); len(status.Servers) != 1 || status.Servers[0].Owner != "knowledge-plugin:1" {
		t.Fatalf("ownership missing: %+v", status)
	}
	if err := fleet.Release(t.Context(), "knowledge", "knowledge-plugin:1"); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Status().Servers) != 0 {
		t.Fatal("plugin disposal retained MCP")
	}
}

func TestStoppedHostMCPStatusDoesNotStartHost(t *testing.T) {
	store := &memoryStore{records: map[string]Account{"alice": {DesiredState: stopped}}}
	controller := New(config.Default(), nil, store)
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, mcpconfig.RuntimePath, 200},
		{http.MethodPost, mcpconfig.RuntimePath + "/servers/test/restart", 409},
		{http.MethodPost, mcpconfig.RuntimePath + "/servers/test/probe", 409},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set(UserHeader, "alice")
		w := httptest.NewRecorder()
		controller.Handler().ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
}

func TestRuntimeMCPProbeDoesNotWaitForAccountMutationLock(t *testing.T) {
	host := httptest.NewServer(httpapi.Authenticate("probe-token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != mcpconfig.InternalPath+"/servers/test/probe" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, mcpconfig.ServerStatus{Name: "test", Source: "plugin", State: "connected", ToolCount: 1})
	})))
	defer host.Close()
	store := &memoryStore{records: map[string]Account{"alice": {DesiredState: running, HostEndpoint: host.URL, HostToken: "probe-token"}}}
	controller := New(config.Default(), nil, store)
	lock := controller.accountLock("alice")
	lock.Lock()
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodPost, mcpconfig.RuntimePath+"/servers/test/probe", nil)
		r.Header.Set(UserHeader, "alice")
		w := httptest.NewRecorder()
		controller.Handler().ServeHTTP(w, r)
		finished <- w
	}()
	select {
	case response := <-finished:
		lock.Unlock()
		if response.Code != 200 {
			t.Fatalf("probe=%d %s", response.Code, response.Body)
		}
	case <-time.After(2 * time.Second):
		lock.Unlock()
		<-finished
		t.Fatal("runtime probe was serialized behind the account mutation lock")
	}
}
