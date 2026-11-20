package mcpclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	mcpconfig "go-bot/mcp"
	"golang.org/x/oauth2"
)

const RequestTimeout = 60 * time.Second
const CloseTimeout = 5 * time.Second

type Error struct {
	Code, Message, AuthorizationReason string
	// Keep protocol error identity for lifecycle decisions without exposing
	// the provider's raw response through public error serialization.
	Cause error `json:"-"`
}

func (e *Error) Error() string                   { return e.Message }
func (e *Error) Unwrap() error                   { return e.Cause }
func (e *Error) ToolErrorInfo() (string, string) { return "McpError", e.Code }
func fail(code, message string) error            { return &Error{Code: code, Message: message} }
func authorizationFailure(reason, message string) error {
	return &Error{Code: "MCP_AUTHORIZATION_REQUIRED", Message: message, AuthorizationReason: reason}
}

type ConnectionOptions struct {
	// Transport is an in-process test/embedding seam. Deployments use the
	// validated stdio or HTTP configuration, never an extra public listener.
	Transport      sdk.Transport
	HTTPClient     *http.Client
	Secrets        []string
	OAuth          auth.OAuthHandler
	StartupTimeout time.Duration
	// Internal observation of failed negotiation cleanup for one-shot probes.
	connectCleanup func(error)
}

type Connection struct {
	Session            *sdk.ClientSession
	Name               string
	Changes            chan struct{}
	ctx                context.Context
	cancel             context.CancelFunc
	command            *exec.Cmd
	secrets            []string
	secretsMu          sync.RWMutex
	closeOnce          sync.Once
	closeDone          chan struct{}
	closeErr           error
	idle               func()
	needsAuthorization atomic.Bool
	authorizationError atomic.Pointer[Error]
}

func (c *Connection) redact(message string) string {
	c.secretsMu.RLock()
	defer c.secretsMu.RUnlock()
	for _, secret := range c.secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	if len(message) > 4096 {
		message = message[:4096] + "…"
	}
	return message
}

func (c *Connection) rememberOAuthToken(token *oauth2.Token) {
	if token == nil {
		return
	}
	c.secretsMu.Lock()
	defer c.secretsMu.Unlock()
	for _, value := range []string{token.AccessToken, token.RefreshToken} {
		if value == "" {
			continue
		}
		found := false
		for _, previous := range c.secrets {
			found = found || previous == value
		}
		if !found {
			c.secrets = append(c.secrets, value)
		}
	}
}

func (c *Connection) requestError(ctx context.Context, err error) error {
	if c.needsAuthorization.Load() {
		return c.authorizationFailure()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if c.ctx.Err() != nil || errors.Is(err, sdk.ErrConnectionClosed) {
		return fail("MCP_SERVER_UNAVAILABLE", "MCP server is disconnected")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fail("MCP_REQUEST_TIMEOUT", "MCP request timed out")
	}
	var protocol *jsonrpc.Error
	if errors.As(err, &protocol) {
		return fail("MCP_PROTOCOL_ERROR", c.redact(err.Error()))
	}
	return fail("MCP_REQUEST_FAILED", c.redact(err.Error()))
}

type trackedTransport struct {
	base       sdk.Transport
	connection sdk.Connection
}

func (t *trackedTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	connection, err := t.base.Connect(ctx)
	t.connection = connection
	return connection, err
}

// Connect initializes a real SDK session before returning. Cancelling an
// initialization cannot leave a child or half-open transport behind. The
// returned connection outlives this management request and is closed by Host.
func Connect(ctx context.Context, name string, config mcpconfig.Server, options ConnectionOptions) (*Connection, error) {
	if !mcpconfig.ValidServerName(name) {
		return nil, fail("MCP_INVALID_CONFIG", "Invalid configured MCP server name")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.OAuth != nil && options.OAuth == nil {
		return nil, authorizationFailure("missing", "MCP server requires an explicit OAuth connection")
	}
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &Connection{Name: name, Changes: make(chan struct{}, 1), ctx: lifetime, cancel: cancel, secrets: append([]string(nil), options.Secrets...), closeDone: make(chan struct{})}
	c.secrets = append(c.secrets, config.SecretValues()...)
	if options.OAuth != nil {
		options.OAuth = &observedOAuthHandler{base: options.OAuth, issued: c.rememberOAuthToken, required: func(err error) {
			var detail *Error
			if errors.As(err, &detail) {
				c.authorizationError.Store(detail)
			}
			c.needsAuthorization.Store(true)
			c.cancel()
		}}
	}
	var transport sdk.Transport = options.Transport
	if transport == nil {
		switch config.Transport {
		case "stdio":
			command := exec.Command(config.Command, config.Args...)
			command.Dir = config.CWD
			command.Env = processEnvironment(config.Env)
			command.Stderr = io.Discard
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			c.command = command
			transport = &sdk.CommandTransport{Command: command, TerminateDuration: time.Second}
		case "streamable-http":
			base := http.DefaultTransport.(*http.Transport).Clone()
			c.idle = base.CloseIdleConnections
			client := &http.Client{Transport: base}
			if options.HTTPClient != nil {
				copy := *options.HTTPClient
				client = &copy
			}
			if client.Transport == nil {
				client.Transport = base
			}
			endpoint, _ := url.Parse(config.URL)
			client.Transport = &headerTransport{base: client.Transport, origin: endpoint, headers: config.Headers}
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			client.Timeout = 0 // Per-call contexts bound requests; notification streams remain live.
			transport = &sdk.StreamableClientTransport{Endpoint: config.URL, HTTPClient: client, OAuthHandler: options.OAuth, MaxRetries: -1, MaxEventSize: 8 << 20}
		}
	}
	tracked := &trackedTransport{base: transport}
	changed := func() {
		select {
		case c.Changes <- struct{}{}:
		default:
		}
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "go-bot", Version: "0.0.1"}, &sdk.ClientOptions{
		Logger:                     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Capabilities:               &sdk.ClientCapabilities{},
		ToolListChangedHandler:     func(context.Context, *sdk.ToolListChangedRequest) { changed() },
		ResourceListChangedHandler: func(context.Context, *sdk.ResourceListChangedRequest) { changed() },
	})
	budget := options.StartupTimeout
	if budget <= 0 {
		budget = RequestTimeout
	}
	startup, stop := context.WithTimeout(ctx, budget)
	defer stop()
	session, err := client.Connect(startup, tracked, nil)
	if err != nil {
		cancel()
		// Connect may fail after a transport starts but before returning its
		// ClientSession. Track that transport so this branch also owns cleanup.
		var cleanupErr error
		if tracked.connection != nil {
			cleanupErr = tracked.connection.Close()
		}
		c.killGroup()
		if c.idle != nil {
			c.idle()
		}
		if options.connectCleanup != nil {
			options.connectCleanup(cleanupErr)
		}
		var authorization *Error
		if c.needsAuthorization.Load() {
			return nil, c.authorizationFailure()
		}
		if errors.As(err, &authorization) && authorization.Code == "MCP_AUTHORIZATION_REQUIRED" {
			return nil, authorization
		}
		return nil, &Error{Code: "MCP_CONNECT_FAILED", Message: c.redact(err.Error()), Cause: err}
	}
	c.Session = session
	go func() { _ = session.Wait(); c.cancel() }()
	return c, nil
}

func processEnvironment(extra map[string]string) []string {
	var environment []string
	for _, key := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TZ", "TMPDIR", "DSH_HOME", "DSH_AGENTS_HOME", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS"} {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	for key, value := range extra {
		environment = append(environment, key+"="+value)
	}
	return environment
}

type headerTransport struct {
	base    http.RoundTripper
	origin  *url.URL
	headers map[string]string
}

func (t *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !strings.EqualFold(request.URL.Host, t.origin.Host) || request.URL.Scheme != t.origin.Scheme {
		return nil, errors.New("MCP transport refused an unexpected origin")
	}
	copy := request.Clone(request.Context())
	copy.Header = request.Header.Clone()
	for key, value := range t.headers {
		copy.Header.Set(key, value)
	}
	return t.base.RoundTrip(copy)
}

func (c *Connection) killGroup() {
	if c.command != nil && c.command.Process != nil {
		_ = syscall.Kill(-c.command.Process.Pid, syscall.SIGKILL)
	}
}

// A cleanup timeout remains an error. Callers must keep the server namespace
// reserved until Closed fires instead of overlapping another process.
func (c *Connection) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		go func() {
			c.closeErr = c.Session.Close()
			// OAuth expiry can prevent the best-effort remote session DELETE.
			// A completed local HTTP transport close still releases our client;
			// it must not prevent deleting the local grant and reconnecting.
			if c.command == nil && (authorizationRequired(c.closeErr) || errors.Is(c.closeErr, context.Canceled)) {
				c.closeErr = nil
			}
			// CommandTransport terminates a child that does not exit after stdin
			// closes. Completed intentional cleanup is not a failed connection.
			var exit *exec.ExitError
			if c.command != nil && errors.As(c.closeErr, &exit) {
				if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() && (status.Signal() == syscall.SIGTERM || status.Signal() == syscall.SIGKILL) {
					c.closeErr = nil
				}
			}
			c.killGroup()
			if c.idle != nil {
				c.idle()
			}
			close(c.closeDone)
		}()
	})
	timer := time.NewTimer(CloseTimeout)
	defer timer.Stop()
	select {
	case <-c.closeDone:
		if c.closeErr != nil {
			return fail("MCP_CLEANUP_FAILED", c.redact(c.closeErr.Error()))
		}
		return nil
	case <-timer.C:
		c.killGroup()
		return fail("MCP_CLEANUP_TIMEOUT", "MCP connection cleanup has not finished")
	}
}
func (c *Connection) Closed() <-chan struct{}       { return c.closeDone }
func (c *Connection) Disconnected() <-chan struct{} { return c.ctx.Done() }

func (c *Connection) request(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	stop := context.AfterFunc(c.ctx, cancel)
	if c.ctx.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

func (c *Connection) Call(ctx context.Context, name string, arguments any) (*sdk.CallToolResult, error) {
	if c.ctx.Err() != nil {
		return nil, fail("MCP_SERVER_UNAVAILABLE", "MCP server is disconnected")
	}
	call, done := c.request(ctx)
	defer done()
	result, err := c.Session.CallTool(call, &sdk.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return nil, c.requestError(ctx, err)
	}
	return result, nil
}
