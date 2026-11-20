package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	mcpconfig "go-bot/mcp"
)

// GetMCPStatus reads plugin-owned connections. Configure connections through
// the owning plugin; there is no independent account MCP configuration.
func (c Client) GetMCPStatus(ctx context.Context) (view mcpconfig.View, err error) {
	err = c.readJSON(ctx, http.MethodGet, mcpconfig.RuntimePath, nil, &view)
	return
}
func (c Client) RestartMCPServer(ctx context.Context, name string) (view mcpconfig.View, err error) {
	err = c.readJSON(ctx, http.MethodPost, mcpconfig.RuntimePath+"/servers/"+url.PathEscape(name)+"/restart", nil, &view)
	return
}

func (c Client) ProbeMCPServer(ctx context.Context, name string) (result mcpconfig.ServerStatus, err error) {
	err = c.readJSON(ctx, http.MethodPost, mcpconfig.RuntimePath+"/servers/"+url.PathEscape(name)+"/probe", nil, &result)
	return
}

func (c Client) AuthorizeMCPServer(ctx context.Context, name, origin string) (view mcpconfig.OAuthAuthorization, err error) {
	err = c.readJSON(ctx, http.MethodPost, mcpconfig.RuntimePath+"/servers/"+url.PathEscape(name)+"/authorize", map[string]string{"origin": origin}, &view)
	return
}
func (c Client) CancelMCPAuthorization(ctx context.Context, name string) (view mcpconfig.View, err error) {
	err = c.readJSON(ctx, http.MethodDelete, mcpconfig.RuntimePath+"/servers/"+url.PathEscape(name)+"/authorization", nil, &view)
	return
}
func (c Client) CancelMCPAuthorizationAttempt(ctx context.Context, name, id string) (view mcpconfig.View, err error) {
	if id == "" {
		return view, errors.New("authorization attempt ID is required")
	}
	err = c.readJSON(ctx, http.MethodDelete, mcpconfig.RuntimePath+"/servers/"+url.PathEscape(name)+"/authorization?id="+url.QueryEscape(id), nil, &view)
	return
}
func (c Client) DisconnectMCPOAuth(ctx context.Context, name string) (view mcpconfig.View, err error) {
	err = c.readJSON(ctx, http.MethodDelete, mcpconfig.RuntimePath+"/servers/"+url.PathEscape(name)+"/grant", nil, &view)
	return
}
