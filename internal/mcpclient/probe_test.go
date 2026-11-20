package mcpclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	mcpconfig "go-bot/mcp"
)

func TestProbeDoesNotPublishToolsOrReplaceManagedGeneration(t *testing.T) {
	manager, _, _ := managedFixture(t, "eager")
	scope := manager.NewScope()
	defer scope.Close()
	tool := findTool(t, scope, PublicToolName("fixture", "one"))
	before := manager.Status()
	result, err := manager.Probe(t.Context(), "fixture")
	if err != nil || result.State != "connected" || result.ToolCount != 2 || !result.Resources || result.ProtocolVersion == "" {
		t.Fatalf("probe=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(before, manager.Status()) || findTool(t, scope, PublicToolName("fixture", "one")) != tool {
		t.Fatal("probe mutated managed state or replaced an active handler")
	}
}

func TestDisabledEntryProbeClosesItsTemporarySessionAndPublishesNothing(t *testing.T) {
	server := testServer()
	remote := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	defer remote.Close()
	manager := NewManager(t.Context(), ManagerOptions{})
	defer manager.Close()
	disabled := false
	if _, err := manager.Apply(t.Context(), mcpconfig.Settings{Servers: map[string]mcpconfig.Server{"disabled": {Enabled: &disabled, Transport: "streamable-http", URL: remote.URL}}}); err != nil {
		t.Fatal(err)
	}
	before := manager.Status()
	result, err := manager.Probe(t.Context(), "disabled")
	if err != nil || result.State != "connected" || result.ToolCount != 2 {
		t.Fatalf("probe=%+v err=%v", result, err)
	}
	scope := manager.NewScope()
	defer scope.Close()
	tools, err := scope.Tools(t.Context(), "")
	if err != nil || len(tools) != 0 || !reflect.DeepEqual(before, manager.Status()) {
		t.Fatal("disabled probe activated persistent tools or state", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		count := 0
		for range server.Sessions() {
			count++
		}
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("temporary MCP session was not closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestProbeWaitsForCredentialsWithoutRequestsOrImplicitOAuth(t *testing.T) {
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected request", 500) }))
	defer remote.Close()
	manager := NewManager(t.Context(), ManagerOptions{})
	defer manager.Close()
	settings := mcpconfig.Settings{Servers: map[string]mcpconfig.Server{
		"key":   {Transport: "streamable-http", URL: remote.URL, Headers: map[string]string{"Authorization": "Bearer ${credential:key}"}},
		"oauth": {Transport: "streamable-http", URL: remote.URL, OAuth: &mcpconfig.OAuth{ClientID: "client"}},
	}}
	if _, err := manager.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"key": "waiting-credentials", "oauth": "waiting-authorization"} {
		result, err := manager.Probe(t.Context(), name)
		if err != nil || result.State != want {
			t.Fatalf("%s probe=%+v err=%v", name, result, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("a waiting probe attempted a connection or login")
	}
}

func TestRemovingServerCancelsProbeAndWaitsForTemporaryCleanup(t *testing.T) {
	server := testServer()
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	started := make(chan struct{}, 1)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			data, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(data)))
			var message struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(data, &message)
			if message.Method == "tools/list" {
				started <- struct{}{}
				<-r.Context().Done()
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer remote.Close()
	manager := NewManager(t.Context(), ManagerOptions{})
	defer manager.Close()
	disabled := false
	if _, err := manager.Apply(t.Context(), mcpconfig.Settings{Servers: map[string]mcpconfig.Server{"probe": {Enabled: &disabled, Transport: "streamable-http", URL: remote.URL}}}); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, err := manager.Probe(t.Context(), "probe"); finished <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not reach discovery")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := manager.Apply(ctx, mcpconfig.Settings{}); err != nil {
		t.Fatal("removal did not drain the probe", err)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("probe did not settle after removal")
	}
	if len(manager.Status().Servers) != 0 {
		t.Fatal("removed server or temporary probe stayed registered")
	}
}
