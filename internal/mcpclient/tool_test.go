package mcpclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go-bot/harness/attachments"
	mcpconfig "go-bot/mcp"
)

func TestMCPToolKeepsOrderedImageAttachmentsStructuredValuesAndServerFailures(t *testing.T) {
	var pixels bytes.Buffer
	canvas := image.NewRGBA(image.Rect(0, 0, 3, 3))
	canvas.Set(1, 1, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&pixels, canvas); err != nil {
		t.Fatal(err)
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	var calls atomic.Int32
	server.AddTool(&sdk.Tool{Name: "render.report", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"fail": map[string]any{"type": "boolean"}}, "additionalProperties": false}, OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []string{"ok"}}}, func(_ context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		calls.Add(1)
		if request.Params.Name != "render.report" {
			t.Error("public name was sent on the wire")
		}
		var input struct {
			Fail bool `json:"fail"`
		}
		_ = json.Unmarshal(request.Params.Arguments, &input)
		text := "before"
		if input.Fail {
			text = "rejected fixture-secret"
		}
		return &sdk.CallToolResult{IsError: input.Fail, StructuredContent: map[string]any{"ok": true}, Content: []sdk.Content{&sdk.TextContent{Text: text}, &sdk.ImageContent{MIMEType: "image/png", Data: pixels.Bytes()}, &sdk.TextContent{Text: "after"}}}, nil
	})
	httpServer := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	t.Cleanup(httpServer.Close)
	connection, err := Connect(t.Context(), "reports", mcpconfig.Server{Transport: "streamable-http", URL: httpServer.URL}, ConnectionOptions{Secrets: []string{"fixture-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	discovery, err := connection.Discover(t.Context())
	if err != nil || len(discovery.Tools) != 1 {
		t.Fatalf("discovery=%+v err=%v", discovery, err)
	}
	store := attachments.Store{API: attachments.LocalObjectAPI{Root: t.TempDir()}}
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	tool := NewTool(connection, discovery.Tools[0], ToolOptions{Images: store, Lifetime: lifetime})
	result, err := tool.ExecuteResult(t.Context(), json.RawMessage(`{}`))
	if err != nil || result.IsError || len(result.ContentBlocks) != 3 || result.ContentBlocks[0].Text != "before" || result.ContentBlocks[1].Type != "image" || result.ContentBlocks[2].Text != "after" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(string(result.Meta), `"structuredContent":{"ok":true}`) || strings.Contains(string(result.Meta), base64.StdEncoding.EncodeToString(pixels.Bytes())) {
		t.Fatal("canonical metadata lost structured output or duplicated image bytes")
	}
	ref := result.ContentBlocks[1]
	if ref.ImageRefID == "" || ref.Width != 3 || ref.Height != 3 {
		t.Fatalf("image attachment=%+v", ref)
	}
	failed, err := tool.ExecuteResult(t.Context(), json.RawMessage(`{"fail":true}`))
	if err != nil || !failed.IsError || failed.Code != "MCP_TOOL_ERROR" || len(failed.ContentBlocks) != 3 || strings.Contains(failed.ContentBlocks[0].Text, "fixture-secret") {
		t.Fatalf("failure=%+v err=%v", failed, err)
	}
	if _, err = tool.ExecuteResult(t.Context(), json.RawMessage(`{"fail":"not-a-boolean"}`)); err == nil {
		t.Fatal("invalid tool arguments were sent")
	}
	if calls.Load() != 2 {
		t.Fatalf("remote calls=%d", calls.Load())
	}
	cancel()
	if _, err = tool.ExecuteResult(t.Context(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("withdrawn generation was still callable")
	}
}

func TestMCPToolValidatesDeclaredStructuredOutput(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "bad", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object", "required": []string{"required"}}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "looks fine"}}, StructuredContent: map[string]any{}}, nil
	})
	httpServer := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	t.Cleanup(httpServer.Close)
	connection, err := Connect(t.Context(), "schemas", mcpconfig.Server{Transport: "streamable-http", URL: httpServer.URL}, ConnectionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	discovery, err := connection.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewTool(connection, discovery.Tools[0], ToolOptions{}).ExecuteResult(t.Context(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("rendered text hid invalid structured output")
	}
}
