package mcpclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"go-bot/internal/credentials"
)

// Registrations outlive grants, including a cancelled browser flow. The key
// binds resource, issuer, redirect URI and configured client ID/scope.
type oauthClientRegistration struct {
	Issuer   string                              `json:"issuer"`
	Response *oauthex.ClientRegistrationResponse `json:"information"`
}

func (m *Manager) oauthClientKey(entry *serverEntry, issuer, redirect string) string {
	var clientID *string
	if entry.config.OAuth.ClientID != "" {
		clientID = &entry.config.OAuth.ClientID
	}
	// JSON.stringify([serverUrl, issuer, redirectUri, clientId, scope])
	// uses null for omitted optional array values and does not HTML-escape URLs.
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode([]any{entry.config.URL, issuer, redirect, clientID, entry.config.OAuth.RequestedScope()})
	data := bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
	sum := sha256.Sum256(data)
	return oauthGrantKey(entry.credentialKey()) + "-client-" + hex.EncodeToString(sum[:])
}

func (s *oauthGrantStore) client(key string) (oauthClientRegistration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readClient(key)
}

// Reads may run inside the shared credential mutation callback. They inspect
// committed data without reentering the mutation queue or its admission lock.
func (s *oauthGrantStore) readClient(key string) (oauthClientRegistration, bool, error) {
	record, err := s.credentials.ReadRecord(oauthClientRecordKey(key))
	var registration oauthClientRegistration
	if err != nil || record == nil {
		return registration, false, err
	}
	if err = decodeOAuthRecord(record, "mcp-oauth-client", &registration); err != nil {
		return registration, false, authorizationFailure("binding", "Stored OAuth client information is incompatible")
	}
	if registration.Response == nil || registration.Response.ClientID == "" || !validOAuthURL(registration.Issuer) {
		return registration, false, fail("MCP_OAUTH_STORE_INVALID", "Stored OAuth client registration is incomplete")
	}
	return registration, true, nil
}

func (s *oauthGrantStore) saveClient(ctx context.Context, key string, registration oauthClientRegistration) error {
	if registration.Response == nil || registration.Response.ClientID == "" || !validOAuthURL(registration.Issuer) {
		return fail("MCP_OAUTH_STORE_INVALID", "Cannot persist incomplete OAuth client registration")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := oauthRecord("mcp-oauth-client", registration)
	if err != nil {
		return err
	}
	_, err = s.credentials.ModifyRecord(ctx, oauthClientRecordKey(key), func(current *credentials.Record) (*credentials.Record, error) {
		if current != nil {
			var previous oauthClientRegistration
			if err := decodeOAuthRecord(current, "mcp-oauth-client", &previous); err != nil {
				return nil, fail("MCP_OAUTH_FAILED", "MCP OAuth client record is incompatible")
			}
		}
		return record, nil
	})
	return err
}

func (s *oauthGrantStore) invalidateClient(ctx context.Context, key string, known oauthClientRegistration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credentials.UpdateRecords(ctx, func(records map[string]credentials.Record) error {
		record, found := records[oauthClientRecordKey(key)]
		if !found {
			return nil
		}
		var current oauthClientRegistration
		if err := decodeOAuthRecord(&record, "mcp-oauth-client", &current); err != nil {
			return err
		}
		if reflect.DeepEqual(current, known) {
			delete(records, oauthClientRecordKey(key))
		}
		return nil
	})
}

// The official SDK has no registration persistence hook. This transport caches
// only its DCR exchange, preserving the SDK's original registration response and
// auth-method selection. Discovery, validation, PKCE and exchange remain SDK-owned.
// A new response is persisted only when the SDK admits it to its browser callback.
type oauthRegistrationTransport struct {
	base               http.RoundTripper
	store              *oauthGrantStore
	key                func(string) string
	resource, redirect string
	remember           func(string)
	mu                 sync.Mutex
	metadata           oauthex.AuthServerMeta
	metadataDiscovered bool
	metadataRaw        json.RawMessage
	preregistered      *oauthex.ClientCredentials
	used               *oauthClientRegistration
	usedKey            string
	fromCache          bool
}

func (m *Manager) registrationClient(entry *serverEntry, redirect string) (*http.Client, *oauthRegistrationTransport) {
	client := m.oauthHTTPClient(entry.config.OAuth)
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	u, _ := url.Parse(entry.config.URL)
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	issuer := u.String()
	cache := &oauthRegistrationTransport{base: base, store: m.oauthStore, resource: entry.config.URL, redirect: redirect, key: func(issuer string) string { return m.oauthClientKey(entry, issuer, redirect) },
		metadata: oauthex.AuthServerMeta{Issuer: issuer, AuthorizationEndpoint: issuer + "/authorize", TokenEndpoint: issuer + "/token", RegistrationEndpoint: issuer + "/register"},
		remember: func(secret string) {
			if secret != "" {
				m.mu.Lock()
				m.secrets[secret] = true
				m.mu.Unlock()
			}
		},
	}
	client.Transport = cache
	return client, cache
}

type replayOAuthBody struct {
	io.Reader
	io.Closer
}

func peekOAuthJSON(response *http.Response) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		response.Body.Close()
		return nil, err
	}
	response.Body = replayOAuthBody{Reader: io.MultiReader(bytes.NewReader(data), response.Body), Closer: response.Body}
	if len(data) > 1<<20 {
		return nil, nil
	}
	return data, nil
}

func (c *oauthRegistrationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	c.mu.Lock()
	metadata := c.metadata
	c.mu.Unlock()
	registrationRequest := request.Method == http.MethodPost && request.URL.String() == metadata.RegistrationEndpoint
	if registrationRequest {
		key := c.key(metadata.Issuer)
		registration, found, err := c.store.client(key)
		if err != nil {
			return nil, err
		}
		if found && registration.Issuer == metadata.Issuer {
			body, err := json.Marshal(registration.Response)
			if err != nil {
				return nil, err
			}
			c.mu.Lock()
			c.used, c.usedKey, c.fromCache = &registration, key, true
			c.mu.Unlock()
			c.remember(registration.Response.ClientSecret)
			return &http.Response{StatusCode: http.StatusCreated, Status: "201 Created", Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: request}, nil
		}
	}
	response, err := c.base.RoundTrip(request)
	if err != nil {
		return response, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, nil
	}
	discovery := request.Method == http.MethodGet && (strings.Contains(request.URL.Path, "/.well-known/oauth-authorization-server") || strings.Contains(request.URL.Path, "/.well-known/openid-configuration"))
	if !registrationRequest && !discovery {
		return response, nil
	}
	data, err := peekOAuthJSON(response)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return response, nil
	}
	if discovery {
		var next oauthex.AuthServerMeta
		if json.Unmarshal(data, &next) == nil && next.Issuer != "" && next.AuthorizationEndpoint != "" && next.TokenEndpoint != "" {
			// The SDK validates this document before it can issue the matching
			// registration request. We do not publish or persist metadata here.
			c.mu.Lock()
			c.metadata = next
			c.metadataDiscovered = true
			c.metadataRaw = append(json.RawMessage(nil), data...)
			c.mu.Unlock()
		}
	} else {
		var registered oauthex.ClientRegistrationResponse
		if json.Unmarshal(data, &registered) == nil && registered.ClientID != "" {
			candidate := oauthClientRegistration{Issuer: metadata.Issuer, Response: &registered}
			c.mu.Lock()
			c.used, c.usedKey, c.fromCache = &candidate, c.key(metadata.Issuer), false
			c.mu.Unlock()
			c.remember(registered.ClientSecret)
		}
	}
	return response, nil
}

// Only metadata actually discovered and subsequently admitted by the SDK is an
// issuer baseline. The fallback endpoints used during discovery are not one.
func (c *oauthRegistrationTransport) callbackIssuer() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.metadataDiscovered {
		return "", false
	}
	return c.metadata.Issuer, c.metadata.AuthorizationResponseIssParameterSupported
}

func (c *oauthRegistrationTransport) authorizationMetadata() (oauthex.AuthServerMeta, json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.metadata, append(json.RawMessage(nil), c.metadataRaw...)
}

func (c *oauthRegistrationTransport) admit(ctx context.Context, authorizationURL string) error {
	c.mu.Lock()
	used, key, cached, metadata := c.used, c.usedKey, c.fromCache, c.metadata
	c.mu.Unlock()
	if used == nil && c.preregistered != nil {
		information := &oauthex.ClientRegistrationResponse{ClientID: c.preregistered.ClientID, ClientRegistrationMetadata: oauthex.ClientRegistrationMetadata{TokenEndpointAuthMethod: "none", RedirectURIs: []string{c.redirect}}}
		if c.preregistered.ClientSecretAuth != nil {
			information.ClientSecret = c.preregistered.ClientSecretAuth.ClientSecret
			information.TokenEndpointAuthMethod = "client_secret_post"
		}
		used = &oauthClientRegistration{Issuer: metadata.Issuer, Response: information}
		key = c.key(metadata.Issuer)
	}
	if used == nil || cached {
		return nil
	}
	actual, err := url.Parse(authorizationURL)
	if err != nil {
		return err
	}
	expected, err := url.Parse(metadata.AuthorizationEndpoint)
	if err != nil {
		return err
	}
	if actual.Scheme != expected.Scheme || actual.Host != expected.Host || actual.Path != expected.Path || actual.Query().Get("client_id") != used.Response.ClientID || actual.Query().Get("redirect_uri") != c.redirect {
		return fail("MCP_OAUTH_CLIENT_BINDING", "OAuth client registration does not match the admitted authorization")
	}
	return c.store.saveClient(ctx, key, *used)
}

func (c *oauthRegistrationTransport) invalidate(ctx context.Context) error {
	c.mu.Lock()
	used, key := c.used, c.usedKey
	c.mu.Unlock()
	if used == nil {
		return nil
	}
	return c.store.invalidateClient(ctx, key, *used)
}
