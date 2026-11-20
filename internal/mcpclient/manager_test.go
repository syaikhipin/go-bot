package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go-bot/harness/agent"
	mcpconfig "go-bot/mcp"
)

func TestRecoveredMCPConnectionDoesNotKeepAFleetUnavailableError(t *testing.T) {
	server := testServer()
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	var online atomic.Bool
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !online.Load() {
			http.Error(w, "temporary QA outage", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	manager := NewManager(t.Context(), ManagerOptions{Connection: ConnectionOptions{StartupTimeout: 300 * time.Millisecond}})
	defer manager.Close()
	settings := mcpconfig.Settings{Servers: map[string]mcpconfig.Server{"recovering": {Transport: "streamable-http", URL: httpServer.URL}}}
	status, err := manager.Apply(t.Context(), settings)
	if err == nil || len(status.Servers) != 1 || status.Servers[0].Error == "" {
		t.Fatalf("initial connection failure was hidden: %+v %v", status, err)
	}
	if status.Error != "" {
		t.Fatalf("one server failure was reported as a fleet availability error: %+v", status)
	}
	online.Store(true)
	status, err = manager.Restart(t.Context(), "recovering")
	if err != nil || len(status.Servers) != 1 || status.Servers[0].State != "connected" || status.Servers[0].Error != "" || status.Error != "" {
		t.Fatalf("recovery retained the old unavailable state: %+v %v", status, err)
	}
	// Invalid resolved configuration has no live entry and remains a fleet
	// configuration diagnostic, rather than being silently discarded.
	settings.Credentials = map[string]string{"header": "invalid\nheader"}
	settings.Servers["invalid"] = mcpconfig.Server{Transport: "streamable-http", URL: httpServer.URL, Headers: map[string]string{"X-QA": "${credential:header}"}}
	status, err = manager.Apply(t.Context(), settings)
	if err == nil || status.Error == "" || status.Servers[0].State != "connected" {
		t.Fatalf("unapplied configuration diagnostic was lost: %+v %v", status, err)
	}
}

func managedFixture(t *testing.T, loading string) (*Manager, *sdk.Server, mcpconfig.Settings) {
	t.Helper()
	server := testServer()
	httpServer := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	t.Cleanup(httpServer.Close)
	manager := NewManager(t.Context(), ManagerOptions{})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	settings := mcpconfig.Settings{Servers: map[string]mcpconfig.Server{"fixture": {Transport: "streamable-http", URL: httpServer.URL, ToolLoading: loading}}}
	status, err := manager.Apply(t.Context(), settings)
	if err != nil || len(status.Servers) != 1 || status.Servers[0].State != "connected" {
		t.Fatalf("initial apply status=%+v err=%v", status, err)
	}
	return manager, server, settings
}

func findTool(t *testing.T, scope *Scope, name string) agent.Tool {
	t.Helper()
	tools, err := scope.Tools(t.Context(), "unused")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Spec().Name == name {
			return tool
		}
	}
	return nil
}

func TestOnDemandToolsBelongToLiveAgentAndLoadAtomically(t *testing.T) {
	manager, _, _ := managedFixture(t, "on-demand")
	first, second := manager.NewScope(), manager.NewScope()
	defer first.Close()
	defer second.Close()
	name := PublicToolName("fixture", "one")
	if findTool(t, first, name) != nil {
		t.Fatal("on-demand tool exposed before loading")
	}
	loader := findTool(t, first, PublicToolName("fixture", "load_tools"))
	if loader == nil {
		t.Fatal("missing loader")
	}
	if _, err := loader.Execute(t.Context(), json.RawMessage(`{"tools":["one","unknown"]}`)); err == nil {
		t.Fatal("unknown tool was accepted")
	}
	if findTool(t, first, name) != nil {
		t.Fatal("failed load partially changed scope")
	}
	message, err := loader.Execute(t.Context(), json.RawMessage(`{"tools":["one","one"]}`))
	if err != nil || message != "Loaded: "+name+"." {
		t.Fatalf("load=%q err=%v", message, err)
	}
	tool := findTool(t, first, name)
	if tool == nil || findTool(t, second, name) != nil {
		t.Fatal("loaded tools leaked across Agent instances")
	}
	if result, err := tool.Execute(t.Context(), json.RawMessage(`{}`)); err != nil || result != "one" {
		t.Fatalf("result=%q err=%v", result, err)
	}
	message, err = loader.Execute(t.Context(), json.RawMessage(`{"tools":["one"]}`))
	if err != nil || message != "Already loaded: "+name+"." {
		t.Fatalf("repeat load=%q err=%v", message, err)
	}
	sections, err := first.Context(t.Context(), "unused")
	if err != nil || len(sections) != 3 || !strings.Contains(sections[1].Text, "{{not_a_template}}") || !strings.Contains(sections[2].Text, "- one: A fixture tool") {
		t.Fatalf("context=%+v err=%v", sections, err)
	}
	first.Close()
	if _, err := loader.Execute(t.Context(), json.RawMessage(`{"tools":["two"]}`)); err == nil {
		t.Fatal("closed scope accepted a load")
	}
}

func TestCatalogChangeWithdrawsOldToolGenerationAndKeepsScopedLoads(t *testing.T) {
	manager, server, settings := managedFixture(t, "on-demand")
	scope := manager.NewScope()
	defer scope.Close()
	loader := findTool(t, scope, PublicToolName("fixture", "load_tools"))
	if _, err := loader.Execute(t.Context(), json.RawMessage(`{"tools":["one"]}`)); err != nil {
		t.Fatal(err)
	}
	old := findTool(t, scope, PublicToolName("fixture", "one"))
	server.AddTool(&sdk.Tool{Name: "three", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	for {
		changes, _ := manager.Changes()
		if manager.Status().Servers[0].ToolCount == 3 {
			break
		}
		select {
		case <-changes:
		case <-deadline.C:
			t.Fatal("manager did not refresh notified catalog")
		}
	}
	if _, err := old.Execute(t.Context(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("withdrawn generation was still callable")
	}
	fresh := findTool(t, scope, PublicToolName("fixture", "one"))
	if fresh == nil || fresh == old {
		t.Fatal("scope lost loaded name or retained stale handler")
	}
	if result, err := fresh.Execute(t.Context(), json.RawMessage(`{}`)); err != nil || result != "one" {
		t.Fatalf("new result=%q err=%v", result, err)
	}
	if _, err := manager.Apply(t.Context(), mcpconfig.Settings{}); err != nil {
		t.Fatal(err)
	}
	if tools, err := scope.Tools(t.Context(), ""); err != nil || len(tools) != 0 {
		t.Fatalf("removed tools=%v err=%v", tools, err)
	}
	if _, err := manager.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
}

func TestManagedResourcesAndOwnershipAndWaitingStates(t *testing.T) {
	manager, _, settings := managedFixture(t, "eager")
	scope := manager.NewScope()
	defer scope.Close()
	list := findTool(t, scope, "list_mcp_resources")
	result, err := list.(agent.ResultTool).ExecuteResult(t.Context(), json.RawMessage(`{"server":"fixture"}`))
	if err != nil || !strings.Contains(string(result.Meta), "memo://one") || !strings.Contains(string(result.Meta), "memo://two") {
		t.Fatalf("resources=%+v err=%v", result, err)
	}
	read := findTool(t, scope, "read_mcp_resource")
	result, err = read.(agent.ResultTool).ExecuteResult(t.Context(), json.RawMessage(`{"server":"fixture","uri":"memo://one"}`))
	if err != nil || len(result.ContentBlocks) != 1 || !strings.Contains(result.ContentBlocks[0].Text, "resource content") {
		t.Fatalf("read=%+v err=%v", result, err)
	}
	if err := manager.Release(t.Context(), "fixture", "wrong-owner"); err == nil {
		t.Fatal("plugin removed user-owned server")
	}
	pluginConfig := settings.Servers["fixture"]
	if err := manager.Register(t.Context(), "owned", "instance-a", pluginConfig); err != nil {
		t.Fatal(err)
	}
	settings.Servers["owned"] = pluginConfig
	if _, err := manager.Apply(t.Context(), settings); err == nil {
		t.Fatal("user overwrote plugin-owned namespace")
	}
	if err := manager.Release(t.Context(), "owned", "instance-b"); err == nil {
		t.Fatal("wrong plugin removed server")
	}
	if err := manager.Release(t.Context(), "owned", "instance-a"); err != nil {
		t.Fatal(err)
	}
	settings.Servers = map[string]mcpconfig.Server{
		"key":   {Transport: "streamable-http", URL: pluginConfig.URL, Headers: map[string]string{"Authorization": "Bearer ${credential:key}"}},
		"oauth": {Transport: "streamable-http", URL: pluginConfig.URL, OAuth: &mcpconfig.OAuth{ClientID: "fixture"}},
	}
	status, err := manager.Apply(t.Context(), settings)
	if err != nil || len(status.Servers) != 2 || status.Servers[0].State != "waiting-credentials" || status.Servers[1].State != "waiting-authorization" {
		t.Fatalf("waiting status=%+v err=%v", status, err)
	}
	if _, err := manager.Restart(t.Context(), "key"); err != nil || manager.Status().Servers[0].State != "waiting-credentials" {
		t.Fatalf("restart lost credential waiting state: %v", err)
	}
}
