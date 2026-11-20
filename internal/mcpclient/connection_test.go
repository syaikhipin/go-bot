package mcpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	mcpconfig "go-bot/mcp"
)

func testServer() *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "remote-name-is-not-our-namespace", Version: "1.0.0"}, &sdk.ServerOptions{PageSize: 1, Instructions: "Literal server instruction: {{not_a_template}}"})
	for _, name := range []string{"one", "two"} {
		server.AddTool(&sdk.Tool{Name: name, Description: "A fixture tool", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: request.Params.Name}}}, nil
		})
	}
	for _, name := range []string{"one", "two"} {
		server.AddResource(&sdk.Resource{Name: name, URI: "memo://" + name}, func(_ context.Context, request *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: request.Params.URI, MIMEType: "text/plain", Text: "resource content"}}}, nil
		})
	}
	server.AddResourceTemplate(&sdk.ResourceTemplate{Name: "memo", URITemplate: "memo://{name}"}, func(_ context.Context, request *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: request.Params.URI, Text: "template content"}}}, nil
	})
	return server
}

func TestMCPHTTPDiscoversEveryPageCallsRawNamesAndReadsResources(t *testing.T) {
	server := testServer()
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	var authenticated atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-private" {
			t.Error("configured credential was not supplied")
			w.WriteHeader(401)
			return
		}
		authenticated.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	connection, err := Connect(t.Context(), "local_namespace", mcpconfig.Server{Transport: "streamable-http", URL: httpServer.URL, Headers: map[string]string{"Authorization": "Bearer fixture-private"}}, ConnectionOptions{Secrets: []string{"fixture-private"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	discovery, err := connection.Discover(t.Context())
	if err != nil || len(discovery.Tools) != 2 || !discovery.Resources || !strings.Contains(discovery.Instructions, "{{not_a_template}}") {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	for _, tool := range discovery.Tools {
		if tool.PublicName != "mcp__local_namespace__"+tool.RawName {
			t.Fatal("remote implementation name replaced local namespace")
		}
	}
	result, err := connection.Call(t.Context(), discovery.Tools[0].RawName, map[string]any{})
	if err != nil || result.Content[0].(*sdk.TextContent).Text != discovery.Tools[0].RawName {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	resources, err := connection.Resources(t.Context())
	if err != nil || len(resources) != 2 {
		t.Fatalf("resources=%+v err=%v", resources, err)
	}
	templates, err := connection.ResourceTemplates(t.Context())
	if err != nil || len(templates) != 1 {
		t.Fatalf("templates=%+v err=%v", templates, err)
	}
	read, err := connection.ReadResource(t.Context(), "memo://one")
	if err != nil || len(read.Contents) != 1 || read.Contents[0].Text != "resource content" {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	if authenticated.Load() < 5 {
		t.Fatal("test did not exercise real HTTP MCP requests")
	}
	for {
		select {
		case <-connection.Changes:
			continue
		default:
			goto drained
		}
	}
drained:
	server.AddTool(&sdk.Tool{Name: "three", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	select {
	case <-connection.Changes:
	case <-time.After(3 * time.Second):
		t.Fatal("tool-list notification was not delivered")
	}
	updated, err := connection.Discover(t.Context())
	if err != nil || len(updated.Tools) != 3 {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
}

func TestMCPDoesNotForwardConfiguredHeadersAcrossRedirects(t *testing.T) {
	var reached atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Store(true); w.WriteHeader(500) }))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	connection, err := Connect(t.Context(), "redirect", mcpconfig.Server{Transport: "streamable-http", URL: origin.URL, Headers: map[string]string{"X-Key": "fixture-secret"}}, ConnectionOptions{Secrets: []string{"fixture-secret"}})
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || reached.Load() {
		t.Fatalf("redirect was followed: reached=%v err=%v", reached.Load(), err)
	}
	if strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("transport error exposed a configured credential")
	}
}

func TestMCPStdioFixture(t *testing.T) {
	if os.Getenv("GO_BOT_MCP_STDIO_FIXTURE") != "1" {
		return
	}
	if os.Getenv("MODEL_API_KEY") != "" {
		os.Exit(3)
	}
	if err := testServer().Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestMCPStdioUsesSeparateArgumentsAndMinimalEnvironment(t *testing.T) {
	t.Setenv("MODEL_API_KEY", "must-not-be-inherited")
	connection, err := Connect(t.Context(), "stdio", mcpconfig.Server{Transport: "stdio", Command: os.Args[0], Args: []string{"-test.run=^TestMCPStdioFixture$"}, Env: map[string]string{"GO_BOT_MCP_STDIO_FIXTURE": "1"}, CWD: t.TempDir()}, ConnectionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	discovery, err := connection.Discover(t.Context())
	if err != nil || len(discovery.Tools) != 2 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	if err = connection.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Call(t.Context(), "one", map[string]any{}); err == nil {
		t.Fatal("closed subprocess remained callable")
	}
}

func TestMCPNameNormalizationMatchesUpstreamJavaScript(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"search", "mcp__github__search"},
		{"a.b", "mcp__github__a_b_531a020b8a5f"},
		{"a/b", "mcp__github__a_b_b36899572871"},
		{"天气", "mcp__github_____cc803593d7fc"},
		{"📄", "mcp__github_____7a12b78977cb"},
		{strings.Repeat("x", 100), "mcp__github__xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx_3accac3b9402"},
	} {
		if got := PublicToolName("github", tc.raw); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.raw, got, tc.want)
		}
	}
}
