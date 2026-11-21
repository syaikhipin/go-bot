package plugins

import (
	"context"
	"errors"
	"os"

	"go-bot/harness/skills"
	"go-bot/internal/authorization"
	"go-bot/internal/credentials"
	mcpconfig "go-bot/mcp"
	"go-bot/plugin"
)

type MCPManager interface {
	Register(context.Context, string, string, mcpconfig.Server) error
	Release(context.Context, string, string) error
	ListViews() []mcpconfig.ManagedServerView
	Changes() (<-chan struct{}, <-chan struct{})
	Restart(context.Context, string) (mcpconfig.Status, error)
	CancelAuthorizationAttempt(context.Context, string, string) error
	DisconnectAuthorization(context.Context, string) (mcpconfig.Status, error)
	StartAuthorization(context.Context, string) (mcpconfig.OAuthAttemptView, error)
}
type RuntimeConfig struct {
	UserID           string
	HostID           string
	Node             string
	Main             string
	ProfileDir       string
	Home             string
	DSHHome          string
	SocketParent     string
	ReservedTools    []string
	Authorization    *authorization.Service
	Credentials      *credentials.Store
	MCP              MCPManager
	Skills           *skills.Registry
	Workspace        string
	Log              func(string, string)
	Changed          func()
	ProactiveChanged func() error
}

func runtimeEnvironment() []string {
	var out []string
	for _, name := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS"} {
		if value, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+value)
		}
	}
	return out
}
func fatalDrain(err error) bool {
	var detail *plugin.Error
	return errors.As(err, &detail) && detail.Code == "PLUGIN_DRAIN_TIMEOUT"
}
