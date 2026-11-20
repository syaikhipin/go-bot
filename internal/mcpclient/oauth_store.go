package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go-bot/internal/credentials"
	"golang.org/x/oauth2"
)

// MCP interprets its own opaque payload. The built-in Go credential service
// owns persistence and atomic mutations, exactly as ctx.credentials upstream.
type oauthGrant struct {
	ID, Binding, ServerURL, Issuer, ClientKey string
	ConfiguredClientID, ConfiguredScope       *string
	Config                                    oauth2.Config
	Token                                     oauth2.Token
	Metadata                                  json.RawMessage
	Resource, Scope                           string
	ScopePresent                              bool
	TokenExpiresIn                            *float64
}
type oauthGrantStore struct {
	mu          sync.Mutex
	credentials *credentials.Store
}

func newOAuthGrantStore(path string) (*oauthGrantStore, error) {
	store, err := credentials.Open(path, credentials.Options{})
	if err != nil {
		return nil, err
	}
	return &oauthGrantStore{credentials: store}, nil
}
func oauthGrantKey(name string) string {
	if credentials.ValidKey(name) {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return "mcp/connection-" + hex.EncodeToString(sum[:])
}
func oauthClientRecordKey(key string) string {
	if credentials.ValidKey(key) {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "mcp/client-" + hex.EncodeToString(sum[:])
}
func oauthRecord(kind string, value any) (*credentials.Record, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return nil, errors.New("invalid MCP credential payload")
	}
	payload["version"], payload["type"] = 1, kind
	data, err = json.Marshal(payload)
	return &credentials.Record{Kind: "grant", Payload: data}, err
}
func decodeOAuthRecord(record *credentials.Record, kind string, output any) error {
	if record == nil {
		return nil
	}
	var tag struct {
		Version int    `json:"version"`
		Type    string `json:"type"`
	}
	if record.Kind != "grant" || json.Unmarshal(record.Payload, &tag) != nil || tag.Version != 1 || tag.Type != kind || json.Unmarshal(record.Payload, output) != nil {
		return fail("MCP_OAUTH_STORE_INVALID", "Stored MCP credential record has an invalid format")
	}
	return nil
}
func validStoredGrant(grant oauthGrant) error {
	if _, err := uuid.Parse(grant.ID); err != nil || grant.Token.AccessToken == "" || grant.Token.TokenType == "" || grant.Config.ClientID == "" || !validOAuthURL(grant.ServerURL) || !validOAuthURL(grant.Issuer) || !credentials.ValidKey(grant.ClientKey) || (grant.Resource != "" && !validOAuthURL(grant.Resource)) {
		return fail("MCP_OAUTH_STORE_INVALID", "Stored OAuth grant is incomplete")
	}
	if !validOAuthURL(grant.Config.Endpoint.TokenURL) || !validOAuthURL(grant.Config.Endpoint.AuthURL) {
		return fail("MCP_OAUTH_STORE_INVALID", "Stored OAuth metadata is incomplete")
	}
	return nil
}
func (s *oauthGrantStore) get(name, binding string) (oauthGrant, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var grant oauthGrant
	record, err := s.credentials.ReadRecord(oauthGrantKey(name))
	if err != nil || record == nil {
		return grant, false, err
	}
	if err = decodeOAuthRecord(record, "mcp-oauth-grant", &grant); err != nil {
		return grant, false, err
	}
	if grant.Binding != binding {
		return grant, false, nil
	}
	return grant, true, validStoredGrant(grant)
}
func (s *oauthGrantStore) put(ctx context.Context, name string, grant oauthGrant) error {
	if name == "" || grant.Binding == "" {
		return fail("MCP_OAUTH_STORE_INVALID", "Cannot persist an incomplete OAuth grant")
	}
	if err := validStoredGrant(grant); err != nil {
		return err
	}
	record, err := oauthRecord("mcp-oauth-grant", grant)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.credentials.ModifyRecord(ctx, oauthGrantKey(name), func(*credentials.Record) (*credentials.Record, error) { return record, nil })
	return err
}
func (s *oauthGrantStore) connectionGrant(name, binding string) (oauthGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var grant oauthGrant
	record, err := s.credentials.ReadRecord(oauthGrantKey(name))
	if err != nil {
		return grant, err
	}
	if record == nil {
		return grant, authorizationFailure("missing", "Connect this MCP server with OAuth")
	}
	if err = decodeOAuthRecord(record, "mcp-oauth-grant", &grant); err != nil {
		return grant, authorizationFailure("binding", "Stored OAuth authorization is incompatible; reconnect this server")
	}
	if grant.Binding != binding {
		return grant, authorizationFailure("binding", "OAuth authorization no longer matches this MCP configuration")
	}
	if err := validStoredGrant(grant); err != nil {
		return grant, authorizationFailure("binding", "Stored OAuth authorization is incompatible; reconnect this server")
	}
	return grant, nil
}
func (s *oauthGrantStore) remove(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credentials.DeleteRecord(ctx, oauthGrantKey(name))
}
func sameOAuthToken(left, right oauth2.Token) bool {
	return left.AccessToken == right.AccessToken && left.RefreshToken == right.RefreshToken && left.TokenType == right.TokenType && left.Expiry.Equal(right.Expiry) && left.ExpiresIn == right.ExpiresIn
}

func oauthTokenExpired(token oauth2.Token) bool {
	// Upstream expiresAt is tested against the current time; oauth2.Valid's
	// implicit ten-second safety margin is not part of that contract.
	return token.AccessToken == "" || (!token.Expiry.IsZero() && !token.Expiry.After(time.Now()))
}

// Refresh under the shared provider's mutation queue, as upstream does. Two
// independently created transports bound to the same grant must reuse the
// first rotation rather than send the same refresh token twice.
func (s *oauthGrantStore) refresh(ctx context.Context, name, binding, id, access string, force bool, client *http.Client) (oauthGrant, error) {
	record, err := s.credentials.ModifyRecord(ctx, oauthGrantKey(name), func(record *credentials.Record) (*credentials.Record, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var grant oauthGrant
		if record == nil {
			return nil, authorizationFailure("binding", "OAuth authorization changed; reconnect this server")
		}
		if err := decodeOAuthRecord(record, "mcp-oauth-grant", &grant); err != nil {
			return nil, authorizationFailure("binding", "Stored OAuth authorization is incompatible; reconnect this server")
		}
		if err := validStoredGrant(grant); err != nil {
			return nil, authorizationFailure("binding", "Stored OAuth authorization is incompatible; reconnect this server")
		}
		if grant.Binding != binding || grant.ID != id {
			return nil, authorizationFailure("binding", "OAuth authorization changed; reconnect this server")
		}
		if grant.Token.AccessToken != access || (!force && !oauthTokenExpired(grant.Token)) {
			return nil, nil
		}
		if grant.Token.RefreshToken == "" {
			return nil, authorizationFailure("expired", "OAuth token expired without a refresh token; reconnect this server")
		}
		// Client information belongs to its own credential record upstream. Do
		// not refresh using a secret copied into an old grant after sign-out,
		// replacement, deletion or a plugin changing the registered identity.
		registration, found, err := s.readClient(grant.ClientKey)
		if err != nil || !found || registration.Issuer != grant.Issuer || registration.Response.ClientID != grant.Config.ClientID {
			return nil, authorizationFailure("binding", "OAuth client registration changed; reconnect this server")
		}
		grant.Config.ClientSecret = registration.Response.ClientSecret
		switch registration.Response.TokenEndpointAuthMethod {
		case "none", "client_secret_post":
			grant.Config.Endpoint.AuthStyle = oauth2.AuthStyleInParams
		case "client_secret_basic":
			grant.Config.Endpoint.AuthStyle = oauth2.AuthStyleInHeader
		}
		refreshContext := context.WithValue(ctx, oauth2.HTTPClient, oauthRefreshClient(client, grant.Config.Endpoint.TokenURL, grant.Resource))
		previous := grant.Token
		// oauth2 normally only refreshes expired tokens. A resource server's
		// 401 is also grounds to refresh even before the advertised expiry.
		previous.Expiry = time.Now().Add(-time.Hour)
		next, err := grant.Config.TokenSource(refreshContext, &previous).Token()
		if err != nil {
			return nil, authorizationFailure("rejected", "OAuth token refresh failed; reconnect this server")
		}
		grant.Token = *next
		grant.TokenExpiresIn = oauthTokenExpiresIn(next)
		if scope, ok := next.Extra("scope").(string); ok {
			grant.Scope = scope
			grant.ScopePresent = true
		}
		return oauthRecord("mcp-oauth-grant", grant)
	})
	if err != nil {
		return oauthGrant{}, err
	}
	var grant oauthGrant
	if err = decodeOAuthRecord(record, "mcp-oauth-grant", &grant); err != nil {
		return grant, err
	}
	return grant, validStoredGrant(grant)
}

// oauth2.Config does not expose token-refresh parameters. Preserve the resource
// selected by the SDK's authorization request, as upstream refreshAuthorization
// does, without changing discovery, authentication or token response handling.
func oauthRefreshClient(client *http.Client, endpoint, resource string) *http.Client {
	if resource == "" {
		return client
	}
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = oauthResourceTransport{base: base, endpoint: endpoint, resource: resource}
	return &copy
}

type oauthResourceTransport struct {
	base               http.RoundTripper
	endpoint, resource string
}

func (t oauthResourceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodPost || request.URL.String() != t.endpoint {
		return t.base.RoundTrip(request)
	}
	if request.Body != nil {
		defer request.Body.Close()
	}
	if request.GetBody == nil {
		return nil, errors.New("OAuth refresh request cannot be replayed")
	}
	body, err := request.GetBody()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(data))
	if err != nil {
		return nil, err
	}
	if form.Get("grant_type") != "refresh_token" {
		return t.base.RoundTrip(request)
	}
	form.Set("resource", t.resource)
	encoded := form.Encode()
	clone := request.Clone(request.Context())
	clone.Body = io.NopCloser(strings.NewReader(encoded))
	clone.ContentLength = int64(len(encoded))
	clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(encoded)), nil }
	return t.base.RoundTrip(clone)
}

type persistentOAuthTokenSource struct {
	mu      sync.Mutex
	ctx     context.Context
	store   *oauthGrantStore
	name    string
	binding string
	id      string
	client  *http.Client
	failed  error // Terminal for this connection; explicit reconnect creates a new source.
}

func (s *oauthGrantStore) tokenSource(ctx context.Context, name string, grant oauthGrant, client *http.Client) oauth2.TokenSource {
	if client == nil {
		client = &http.Client{Timeout: RequestTimeout}
	}
	return &persistentOAuthTokenSource{ctx: ctx, store: s, name: name, binding: grant.Binding, id: grant.ID,
		client: client}
}

func (s *persistentOAuthTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return nil, s.failed
	}
	// Like oauth2.ReuseTokenSource, a still-valid cached token remains usable
	// for the transport's final authenticated DELETE after Host cancellation.
	// Refresh requests still carry the cancelled owner context and cannot start
	// fresh network work after shutdown.
	grant, err := s.store.connectionGrant(s.name, s.binding)
	if err == nil && grant.ID != s.id {
		err = authorizationFailure("binding", "OAuth authorization changed; reconnect this server")
	}
	if err != nil {
		s.failed = err
		return nil, s.failed
	}
	if oauthTokenExpired(grant.Token) {
		grant, err = s.store.refresh(s.ctx, s.name, s.binding, s.id, grant.Token.AccessToken, false, s.client)
		if err != nil {
			s.failed = err
			return nil, err
		}
	}
	token := grant.Token
	return &token, nil
}

func (s *persistentOAuthTokenSource) refreshRejected(ctx context.Context, access string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return s.failed
	}
	// Both the connection and the particular rejected request own this refresh.
	request, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	defer cancel()
	if ctx.Err() != nil {
		cancel()
	}
	_, err := s.store.refresh(request, s.name, s.binding, s.id, access, true, s.client)
	if err != nil {
		s.failed = err
		return err
	}
	return nil
}
