package plugins

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	mcpconfig "go-bot/mcp"
)

type hostMCPDefinition struct {
	Name        string           `json:"name"`
	Connection  mcpconfig.Server `json:"connection"`
	ToolLoading string           `json:"toolLoading,omitempty"`
}

func validateHostMCP(row *hostContribution) error {
	var definition hostMCPDefinition
	if err := decodeHostJSON(row.Descriptor, &definition); err != nil {
		return err
	}
	if !mcpconfig.ValidServerName(row.Owner + "__" + definition.Name) {
		return failure("INVALID_ARGUMENT", "The namespaced MCP name must fit within 32 characters")
	}
	if definition.Connection.Enabled != nil {
		return failure("INVALID_ARGUMENT", "MCP registration lifetime is managed by Cordis; enabled is not a connection field")
	}
	if definition.ToolLoading != "" {
		definition.Connection.ToolLoading = definition.ToolLoading
	}
	return definition.Connection.Validate()
}

func (r *hostBridge) registerMCPResource(ctx context.Context, row *hostContribution) (func(context.Context) error, error) {
	if r.config.MCP == nil {
		return nil, failure("UNAVAILABLE", "Go MCP Client is not configured")
	}
	var definition hostMCPDefinition
	_ = json.Unmarshal(row.Descriptor, &definition)
	if definition.ToolLoading != "" {
		definition.Connection.ToolLoading = definition.ToolLoading
	}
	name := row.Owner + "__" + definition.Name
	source := "bridge:" + filepath.Base(r.directory) + ":" + row.Owner
	if err := r.config.MCP.Register(ctx, name, source, definition.Connection); err != nil {
		return nil, err
	}
	return func(ctx context.Context) error { return r.config.MCP.Release(ctx, name, source) }, nil
}

func (r *hostBridge) mcpViews(owner string) []map[string]any {
	views := []map[string]any{}
	for _, view := range r.config.MCP.ListViews() {
		if name, ok := strings.CutPrefix(view.Name, owner+"__"); ok {
			item := map[string]any{"name": name, "state": view.Status.State, "toolCount": view.Status.ToolCount}
			if view.Authorization != nil {
				item["authorization"] = view.Authorization
			}
			if view.Status.LastError != "" {
				message := view.Status.LastError
				if r.config.Credentials != nil {
					message = r.config.Credentials.Redact(message)
				}
				item["error"] = message
			}
			views = append(views, item)
		}
	}
	return views
}

func (r *hostBridge) mcpRequest(ctx context.Context, owner, method string, raw json.RawMessage) (any, error) {
	if r.config.MCP == nil {
		return nil, failure("UNAVAILABLE", "Go MCP Client is not configured")
	}
	var input struct {
		Name      string `json:"name"`
		AttemptID string `json:"attemptId,omitempty"`
	}
	if err := decodeHostJSON(raw, &input); err != nil {
		return nil, err
	}
	if method == "list" {
		return r.mcpViews(owner), nil
	}
	name := owner + "__" + input.Name
	if !mcpconfig.ValidServerName(name) {
		return nil, failure("INVALID_ARGUMENT", "Invalid MCP name")
	}
	view := func() any {
		for _, view := range r.mcpViews(owner) {
			if view["name"] == input.Name {
				return view
			}
		}
		return nil
	}
	if method == "inspect" {
		return view(), nil
	}
	if view() == nil {
		return nil, failure("NOT_FOUND", "MCP service was not registered by this plugin")
	}
	switch method {
	case "restart":
		_, err := r.config.MCP.Restart(ctx, name)
		return view(), err
	case "authorize":
		return r.config.MCP.StartAuthorization(ctx, name)
	case "cancelAuthorization":
		return nil, r.config.MCP.CancelAuthorizationAttempt(ctx, name, input.AttemptID)
	case "signOut":
		_, err := r.config.MCP.DisconnectAuthorization(ctx, name)
		return nil, err
	default:
		return nil, failure("NOT_FOUND", "Unknown MCP method")
	}
}
