package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type ToolDefinition struct {
	PublicName   string
	RawName      string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	input        *jsonschema.Schema
	output       *jsonschema.Schema
}

type Discovery struct {
	Tools           []ToolDefinition
	Instructions    string
	Resources       bool
	ServerInfo      *sdk.Implementation
	ProtocolVersion string
}

type denyLoader struct{}

func (denyLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external MCP schema references are not supported: %s", url)
}

func compileSchema(value any) (json.RawMessage, *jsonschema.Schema, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, nil, err
	}
	if len(data) > 1<<20 {
		return nil, nil, fail("MCP_INVALID_SCHEMA", "MCP tool schema exceeds 1 MiB")
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(denyLoader{})
	const location = "https://go-bot.invalid/mcp-schema.json"
	if err = c.AddResource(location, value); err != nil {
		return nil, nil, err
	}
	schema, err := c.Compile(location)
	return data, schema, err
}

func collectPages[T any](fetch func(string) ([]T, string, error)) ([]T, error) {
	all := []T{}
	cursor := ""
	seen := map[string]bool{}
	for {
		if seen[cursor] {
			return nil, fail("MCP_INVALID_LIST", "MCP server repeated a pagination cursor")
		}
		seen[cursor] = true
		rows, next, err := fetch(cursor)
		if err != nil {
			return nil, err
		}
		if len(all)+len(rows) > 10000 {
			return nil, fail("MCP_LIST_LIMIT", "MCP discovery exceeds 10000 items")
		}
		all = append(all, rows...)
		if next == "" {
			return all, nil
		}
		cursor = next
	}
}

// Discover constructs and validates the entire next definition set. It never
// publishes part of a failed list or mutates an earlier generation.
func (c *Connection) Discover(ctx context.Context) (Discovery, error) {
	call, done := c.request(ctx)
	defer done()
	info := c.Session.InitializeResult()
	result := Discovery{Tools: []ToolDefinition{}, Instructions: info.Instructions, ServerInfo: info.ServerInfo, ProtocolVersion: info.ProtocolVersion, Resources: info.Capabilities != nil && info.Capabilities.Resources != nil}
	if len(result.Instructions) > 32<<10 {
		result.Instructions = result.Instructions[:32<<10]
		for !utf8.ValidString(result.Instructions) {
			result.Instructions = result.Instructions[:len(result.Instructions)-1]
		}
	}
	if info.Capabilities == nil || info.Capabilities.Tools == nil {
		return result, nil
	}
	tools, err := collectPages(func(cursor string) ([]*sdk.Tool, string, error) {
		page, err := c.Session.ListTools(call, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, "", err
		}
		return page.Tools, page.NextCursor, nil
	})
	if err != nil {
		return Discovery{}, c.requestError(ctx, err)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		if tool == nil || tool.Name == "" {
			return Discovery{}, fail("MCP_INVALID_LIST", "MCP server returned an unnamed tool")
		}
		public := PublicToolName(c.Name, tool.Name)
		if names[public] {
			return Discovery{}, fail("MCP_INVALID_LIST", "MCP server returned duplicate tool identities")
		}
		names[public] = true
		input, inputCheck, err := compileSchema(tool.InputSchema)
		if err != nil {
			return Discovery{}, fail("MCP_INVALID_SCHEMA", c.redact(err.Error()))
		}
		var object map[string]any
		if json.Unmarshal(input, &object) != nil || object["type"] != "object" {
			return Discovery{}, fail("MCP_INVALID_SCHEMA", "MCP tool parameters must have an object root")
		}
		definition := ToolDefinition{PublicName: public, RawName: tool.Name, Description: tool.Description, InputSchema: input, input: inputCheck}
		if tool.OutputSchema != nil {
			definition.OutputSchema, definition.output, err = compileSchema(tool.OutputSchema)
			if err != nil {
				return Discovery{}, fail("MCP_INVALID_SCHEMA", c.redact(err.Error()))
			}
		}
		result.Tools = append(result.Tools, definition)
	}
	return result, nil
}

func (c *Connection) Resources(ctx context.Context) ([]*sdk.Resource, error) {
	call, done := c.request(ctx)
	defer done()
	rows, err := collectPages(func(cursor string) ([]*sdk.Resource, string, error) {
		page, err := c.Session.ListResources(call, &sdk.ListResourcesParams{Cursor: cursor})
		if err != nil {
			return nil, "", err
		}
		return page.Resources, page.NextCursor, nil
	})
	if err != nil {
		return nil, c.requestError(ctx, err)
	}
	return rows, nil
}

func (c *Connection) ResourceTemplates(ctx context.Context) ([]*sdk.ResourceTemplate, error) {
	call, done := c.request(ctx)
	defer done()
	rows, err := collectPages(func(cursor string) ([]*sdk.ResourceTemplate, string, error) {
		page, err := c.Session.ListResourceTemplates(call, &sdk.ListResourceTemplatesParams{Cursor: cursor})
		if err != nil {
			return nil, "", err
		}
		return page.ResourceTemplates, page.NextCursor, nil
	})
	if err != nil {
		return nil, c.requestError(ctx, err)
	}
	return rows, nil
}

func (c *Connection) ReadResource(ctx context.Context, uri string) (*sdk.ReadResourceResult, error) {
	if uri == "" || len(uri) > 16384 {
		return nil, fail("MCP_INVALID_RESOURCE", "Enter an MCP resource URI")
	}
	call, done := c.request(ctx)
	defer done()
	result, err := c.Session.ReadResource(call, &sdk.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, c.requestError(ctx, err)
	}
	return result, nil
}
