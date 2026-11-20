// Package mcpconfig defines the built-in MCP contracts used by Host plugins.
// Connections, OAuth grants and discovered tools belong to the Host.
package mcpconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go-bot/internal/credentials"
	"golang.org/x/net/http/httpguts"
)

const InternalPath = "/internal/host/mcp"
const RuntimePath = "/v1/host/mcp"
const OAuthCallbackPath = "/v1/mcp/oauth/callback/"

// OAuthCallbackHeader is private Controller-to-Host routing metadata. It must
// be replaced, never relayed from an external client's supplied value.
const OAuthCallbackHeader = "X-Go-Bot-OAuth-Callback"
const ControllerOriginHeader = "X-Go-Bot-Controller-Origin"

type OAuth struct {
	CredentialKey          string   `json:"credentialKey,omitempty"`
	ClientID               string   `json:"clientId,omitempty"`
	ClientSecretRef        string   `json:"clientSecretRef,omitempty"`
	Scopes                 []string `json:"scopes,omitempty"`
	Scope                  *string  `json:"scope,omitempty"`
	RequestTimeoutMS       *int64   `json:"requestTimeoutMs,omitempty"`
	AuthorizationTimeoutMS *int64   `json:"authorizationTimeoutMs,omitempty"`
	// ClientSecret is resolved Host-private input. It never crosses public
	// settings JSON or Redis; plugin RPC delivers it in a separate envelope.
	ClientSecret string `json:"-"`
}

// RequestedScope preserves the upstream distinction between omitted scope
// (use the server challenge) and an explicitly empty scope.
func (o OAuth) RequestedScope() *string {
	if o.Scope != nil {
		return o.Scope
	}
	if o.Scopes != nil {
		value := strings.Join(o.Scopes, " ")
		return &value
	}
	return nil
}

type Server struct {
	Enabled     *bool             `json:"enabled,omitempty"`
	Transport   string            `json:"transport"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	CWD         string            `json:"cwd,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	ToolLoading string            `json:"toolLoading,omitempty"`
	OAuth       *OAuth            `json:"oauth,omitempty"`
}

type Settings struct {
	Servers     map[string]Server `json:"servers"`
	Credentials map[string]string `json:"credentials"`
}

var serverName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var credentialReference = regexp.MustCompile(`\$\{credential:([^}]+)\}`)

func ValidServerName(name string) bool { return serverName.MatchString(name) }
func (s Server) IsEnabled() bool       { return s.Enabled == nil || *s.Enabled }
func (s Server) Loading() string {
	if s.ToolLoading == "" {
		return "eager"
	}
	return s.ToolLoading
}

func (s Server) Validate() error {
	if s.Loading() != "eager" && s.Loading() != "on-demand" {
		return errors.New("invalid MCP tool loading mode")
	}
	if len(s.Args) > 256 || len(s.Env) > 128 || len(s.Headers) > 64 {
		return errors.New("MCP server configuration exceeds supported limits")
	}
	switch s.Transport {
	case "stdio":
		if s.Command == "" || strings.TrimSpace(s.Command) != s.Command || len(s.Command) > 4096 || strings.ContainsAny(s.Command, "\x00\r\n") || strings.Contains(s.Command, "${credential:") {
			return errors.New("enter a literal MCP executable and separate arguments")
		}
		if s.URL != "" || len(s.Headers) > 0 || s.OAuth != nil {
			return errors.New("stdio cannot configure an HTTP URL, headers or OAuth")
		}
		if s.CWD != "" && (!filepath.IsAbs(s.CWD) || strings.ContainsAny(s.CWD, "\x00\r\n") || strings.Contains(s.CWD, "${credential:")) {
			return errors.New("MCP working directory must be an absolute literal Host path")
		}
	case "streamable-http":
		u, err := url.Parse(s.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || len(s.URL) > 4096 || strings.Contains(s.URL, "${credential:") {
			return errors.New("enter an HTTP(S) MCP URL; put credentials in headers")
		}
		if s.Command != "" || len(s.Args) > 0 || len(s.Env) > 0 || s.CWD != "" {
			return errors.New("HTTP MCP servers cannot configure a process")
		}
	default:
		return errors.New("MCP transport must be stdio or streamable-http")
	}
	for _, arg := range s.Args {
		if len(arg) > 16384 || strings.ContainsRune(arg, 0) {
			return errors.New("invalid MCP command argument")
		}
	}
	for key, value := range s.Env {
		if !envName.MatchString(key) || len(value) > 16384 || strings.ContainsRune(value, 0) {
			return errors.New("invalid MCP process environment")
		}
	}
	seen := map[string]bool{}
	for key, value := range s.Headers {
		lower := strings.ToLower(key)
		if !httpguts.ValidHeaderFieldName(key) || !httpguts.ValidHeaderFieldValue(value) || len(value) > 16384 || seen[lower] {
			return errors.New("invalid or duplicate MCP HTTP header")
		}
		seen[lower] = true
		if s.OAuth != nil && lower == "authorization" {
			return errors.New("choose OAuth or an Authorization header, not both")
		}
	}
	if s.OAuth != nil {
		for _, budget := range []*int64{s.OAuth.RequestTimeoutMS, s.OAuth.AuthorizationTimeoutMS} {
			if budget != nil && (*budget < 1 || *budget > 2147483647) {
				return errors.New("invalid MCP OAuth timeout")
			}
		}
		if s.OAuth.Scope != nil && (len(*s.OAuth.Scope) > 16384 || strings.ContainsAny(*s.OAuth.Scope, "\x00\r\n")) {
			return errors.New("invalid MCP OAuth scope")
		}
		if s.OAuth.CredentialKey != "" && !credentials.ValidKey(s.OAuth.CredentialKey) {
			return errors.New("invalid MCP OAuth credential key")
		}
		if len(s.OAuth.ClientSecret) > 16384 || strings.ContainsRune(s.OAuth.ClientSecret, 0) {
			return errors.New("invalid private MCP OAuth client secret")
		}
		if (s.OAuth.ClientSecretRef != "" || s.OAuth.ClientSecret != "") && s.OAuth.ClientID == "" {
			return errors.New("MCP OAuth clientId is required with a client secret")
		}
		if len(s.OAuth.ClientID) > 4096 || strings.ContainsAny(s.OAuth.ClientID, "\x00\r\n") || len(s.OAuth.ClientSecretRef) > 256 || strings.ContainsAny(s.OAuth.ClientSecretRef, "\x00\r\n") || len(s.OAuth.Scopes) > 64 {
			return errors.New("invalid MCP OAuth client configuration")
		}
		for _, scope := range s.OAuth.Scopes {
			if scope == "" || len(scope) > 256 || strings.ContainsAny(scope, "\x00\r\n\t ") {
				return errors.New("invalid MCP OAuth scope")
			}
		}
	}
	for _, value := range s.templateValues() {
		remaining := credentialReference.ReplaceAllString(value, "")
		if strings.Contains(remaining, "${credential:") {
			return errors.New("invalid MCP credential reference template")
		}
		for _, match := range credentialReference.FindAllStringSubmatch(value, -1) {
			if err := validCredential(match[1], ""); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s Server) templateValues() []string {
	values := append([]string(nil), s.Args...)
	for _, value := range s.Env {
		values = append(values, value)
	}
	for _, value := range s.Headers {
		values = append(values, value)
	}
	return values
}

func sensitiveHeader(name string) bool {
	name = strings.ToLower(name)
	return name == "authorization" || name == "proxy-authorization" || name == "cookie" || name == "apikey" || strings.Contains(name, "api-key") || strings.HasSuffix(name, "-token") || strings.HasSuffix(name, "-secret")
}

// SecretValues also covers literal authentication headers accepted by the
// transport. Referenced values remain in the credential table; placeholders
// themselves are configuration, not secrets to hide from an editor.
func (s Server) SecretValues() []string {
	var values []string
	if s.OAuth != nil && s.OAuth.ClientSecret != "" {
		values = append(values, s.OAuth.ClientSecret)
	}
	for name, value := range s.Headers {
		if !sensitiveHeader(name) || value == "" || credentialReference.MatchString(value) {
			continue
		}
		values = append(values, value)
		fields := strings.Fields(value)
		if strings.EqualFold(name, "authorization") && len(fields) == 2 {
			values = append(values, fields[1])
		}
	}
	return values
}

func validCredential(ref, value string) error {
	if ref == "" || len(ref) > 256 || strings.ContainsAny(ref, "\x00\r\n") || len(value) > 16384 || strings.ContainsRune(value, 0) {
		return errors.New("invalid MCP credential reference or value")
	}
	return nil
}

func (s Settings) Validate() error {
	if len(s.Servers) > 64 || len(s.Credentials) > 256 {
		return errors.New("MCP settings exceed supported limits")
	}
	for name, server := range s.Servers {
		if !ValidServerName(name) {
			return errors.New("MCP server names use 1–32 letters, digits, underscores or hyphens")
		}
		if err := server.Validate(); err != nil {
			return fmt.Errorf("MCP server %s: %w", name, err)
		}
	}
	for ref, value := range s.Credentials {
		if err := validCredential(ref, value); err != nil {
			return err
		}
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(data) > 768<<10 {
		return errors.New("MCP settings exceed 768 KiB")
	}
	return nil
}

func (s Settings) Clone() Settings {
	data, _ := json.Marshal(s)
	var copy Settings
	_ = json.Unmarshal(data, &copy)
	for name, server := range s.Servers {
		if server.OAuth != nil {
			cloned := copy.Servers[name]
			if cloned.OAuth != nil {
				cloned.OAuth.ClientSecret = server.OAuth.ClientSecret
				copy.Servers[name] = cloned
			}
		}
	}
	return copy
}

// Resolve expands only explicit references. Unrelated account credentials do
// not enter the process environment, HTTP headers or a model request.
func (s Settings) Resolve(name string) (Server, []string, error) {
	server, ok := s.Servers[name]
	if !ok {
		return Server{}, nil, errors.New("MCP server not configured")
	}
	if err := server.Validate(); err != nil {
		return Server{}, nil, err
	}
	copy := Settings{Servers: map[string]Server{name: server}}.Clone().Servers[name]
	missing := map[string]bool{}
	expand := func(value string) string {
		return credentialReference.ReplaceAllStringFunc(value, func(token string) string {
			ref := credentialReference.FindStringSubmatch(token)[1]
			value := s.Credentials[ref]
			if value == "" {
				missing[ref] = true
				return token
			}
			return value
		})
	}
	for i, value := range copy.Args {
		copy.Args[i] = expand(value)
	}
	for key, value := range copy.Env {
		copy.Env[key] = expand(value)
	}
	for key, value := range copy.Headers {
		copy.Headers[key] = expand(value)
	}
	if copy.OAuth != nil && copy.OAuth.ClientSecretRef != "" && copy.OAuth.ClientSecret == "" {
		copy.OAuth.ClientSecret = s.Credentials[copy.OAuth.ClientSecretRef]
		if copy.OAuth.ClientSecret == "" {
			missing[copy.OAuth.ClientSecretRef] = true
		}
	}
	var refs []string
	for ref := range missing {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	if len(refs) == 0 {
		if err := copy.Validate(); err != nil {
			return Server{}, nil, errors.New("resolved MCP configuration is invalid")
		}
	}
	return copy, refs, nil
}

func (s Settings) PublicView() Settings {
	copy := s.Clone()
	if copy.Servers == nil {
		copy.Servers = map[string]Server{}
	}
	if copy.Credentials == nil {
		copy.Credentials = map[string]string{}
	}
	for name, server := range copy.Servers {
		if server.OAuth != nil {
			server.OAuth.ClientSecret = ""
		}
		for key, value := range server.Headers {
			if sensitiveHeader(key) && value != "" && !credentialReference.MatchString(value) {
				server.Headers[key] = "[redacted]"
			}
		}
		copy.Servers[name] = server
	}
	for ref, secret := range copy.Credentials {
		copy.Credentials[ref] = ""
		if secret == "" {
			continue
		}
		for name, server := range copy.Servers {
			for i, value := range server.Args {
				server.Args[i] = strings.ReplaceAll(value, secret, "[redacted]")
			}
			for key, value := range server.Env {
				server.Env[key] = strings.ReplaceAll(value, secret, "[redacted]")
			}
			for key, value := range server.Headers {
				server.Headers[key] = strings.ReplaceAll(value, secret, "[redacted]")
			}
			server.URL = strings.ReplaceAll(server.URL, secret, "[redacted]")
			copy.Servers[name] = server
		}
	}
	return copy
}
