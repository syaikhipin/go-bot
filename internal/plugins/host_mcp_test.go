package plugins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go-bot/harness/agent"
	"go-bot/internal/mcpclient"
)

func TestHostBridgeMCPUsesGoConnectionCredentialsAndDirectTool(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "business", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "pong from Go MCP"}}}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-secret" {
			http.Error(w, "missing credential", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer remote.Close()
	var manager *mcpclient.Manager
	r := bridgeFixture(t, `export const inject=['host'];export async function apply(ctx,config){await ctx.host.mcp.register({name:'docs',connection:{transport:'streamable-http',url:config.url,headers:{Authorization:'Bearer ${credential:MCP_API_KEY}'}}});await ctx.host.tools.register({name:'state',description:'Inspect MCP',inputSchema:{type:'object'},async execute(){return{content:[{type:'text',text:JSON.stringify(await ctx.host.mcp.inspect('docs'))}]}}})}`, func(c *RuntimeConfig) {
		if err := c.Credentials.Set(t.Context(), "MCP_API_KEY", "fixture-secret"); err != nil {
			t.Fatal(err)
		}
		manager = mcpclient.NewManager(t.Context(), mcpclient.ManagerOptions{Credentials: c.Credentials})
		t.Cleanup(func() { _ = manager.Close() })
		c.MCP = manager
	})
	config, _ := json.Marshal(map[string]string{"url": remote.URL})
	loaded, err := r.Apply(t.Context(), []hostPluginInstance{{ID: "mail", Package: "fixture", Enabled: true, Config: config}})
	if err != nil || loaded[0]["state"] != "ready" {
		t.Fatalf("load=%v err=%v", loaded, err)
	}
	result := bridgeRunTool(t, r, "mail__state", nil)
	value, err := agent.DecodeToolPipelineResult(result)
	decoded := value.Content
	if err != nil || !strings.Contains(decoded, `"state":"connected"`) || strings.Contains(decoded, "fixture-secret") {
		t.Fatalf("view=%s err=%v", decoded, err)
	}
	scope := manager.NewScope()
	defer scope.Close()
	tools, err := scope.Tools(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools {
		if strings.HasSuffix(tool.Spec().Name, "ping") {
			found = true
			out, err := tool.Execute(t.Context(), json.RawMessage(`{}`))
			if err != nil || out != "pong from Go MCP" {
				t.Fatal(out, err)
			}
		}
	}
	if !found {
		t.Fatal("Go MCP tool was not discovered")
	}
	if _, err = r.Apply(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if len(manager.Status().Servers) != 0 {
		t.Fatal("Cordis disposal retained the Go MCP connection")
	}
}
