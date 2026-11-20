package mcpclient

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	authservice "go-bot/internal/authorization"
	"go-bot/internal/credentials"
	mcpconfig "go-bot/mcp"
	"golang.org/x/oauth2"
)

const OAuthAuthorizationTimeout = 10 * time.Minute

type AuthorizationCommit func(*credentials.Record) error
type oauthAttempt struct {
	entry          *serverEntry
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	ready          chan struct{}
	answer         chan oauthCallbackResult
	state          string
	issuer         string
	issuerRequired bool
	consumed       bool
	deadline       *time.Timer
	committing     bool
	committed      bool
	view           mcpconfig.OAuthAuthorization
	err            error
}

type oauthCallbackResult struct {
	value *auth.AuthorizationResult
	err   error
}

func authorizationRequired(err error) bool {
	var detail *Error
	return errors.As(err, &detail) && detail.Code == "MCP_AUTHORIZATION_REQUIRED"
}

func (m *Manager) oauthConfigBinding(_, _ string, config mcpconfig.Server) string {
	if config.OAuth == nil {
		return ""
	}
	// The credential key selects the owner. Upstream binds the grant only to
	// the resolved endpoint, configured client ID and configured scope;
	// runtime handles, budgets and secret rotation are not new identities.
	var clientID *string
	if config.OAuth.ClientID != "" {
		clientID = &config.OAuth.ClientID
	}
	return oauthIdentityBinding(config.URL, clientID, config.OAuth.RequestedScope())
}

func oauthIdentityBinding(serverURL string, clientID, scope *string) string {
	data, _ := json.Marshal(struct {
		URL             string
		ClientID, Scope *string
	}{serverURL, clientID, scope})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (m *Manager) oauthHTTPClient(options *mcpconfig.OAuth) *http.Client {
	client := &http.Client{}
	if m.options.Connection.HTTPClient != nil {
		copy := *m.options.Connection.HTTPClient
		client = &copy
	}
	client.Timeout = 10 * time.Second
	if options != nil && options.RequestTimeoutMS != nil {
		client.Timeout = time.Duration(*options.RequestTimeoutMS) * time.Millisecond
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func oauthAuthorizationTimeout(options *mcpconfig.OAuth) time.Duration {
	if options != nil && options.AuthorizationTimeoutMS != nil {
		return time.Duration(*options.AuthorizationTimeoutMS) * time.Millisecond
	}
	return OAuthAuthorizationTimeout
}

type storedOAuthHandler struct{ source oauth2.TokenSource }

// The Go SDK otherwise infers requested scopes from resource metadata. The
// upstream MCP owner uses only configured scope or the actual Bearer challenge.
type challengeScopeOAuthHandler struct {
	base    auth.OAuthHandler
	observe func(*http.Response)
}

func (h challengeScopeOAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return h.base.TokenSource(ctx)
}
func (h challengeScopeOAuthHandler) Authorize(ctx context.Context, request *http.Request, response *http.Response) error {
	h.observe(response)
	return h.base.Authorize(ctx, request, response)
}

func (h *storedOAuthHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return h.source, nil
}
func (h *storedOAuthHandler) Authorize(ctx context.Context, request *http.Request, response *http.Response) error {
	if response.Body != nil {
		_ = response.Body.Close()
	}
	if response.StatusCode == http.StatusForbidden {
		challenges, err := oauthex.ParseWWWAuthenticate(response.Header.Values("WWW-Authenticate"))
		stepUp := false
		if err == nil {
			for _, challenge := range challenges {
				if challenge.Scheme == "bearer" && challenge.Params["error"] == "insufficient_scope" {
					stepUp = true
				}
			}
		}
		if !stepUp {
			return nil
		}
	}
	if response.StatusCode == http.StatusForbidden {
		return authorizationFailure("scope", "MCP server requires additional OAuth scopes")
	}
	if source, ok := h.source.(*persistentOAuthTokenSource); ok {
		access, present := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
		if !present || access == "" {
			return fail("MCP_OAUTH_FAILED", "MCP OAuth response has no credential binding")
		}
		return source.refreshRejected(ctx, access)
	}
	return authorizationFailure("rejected", "MCP server requires an explicit OAuth connection")
}

type observedOAuthHandler struct {
	base               auth.OAuthHandler
	required           func(error)
	issued             func(*oauth2.Token)
	authorizeMu        sync.Mutex
	authorizationError error
}

func (h *observedOAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	source, err := h.base.TokenSource(ctx)
	if authorizationRequired(err) {
		h.required(err)
	}
	if source == nil || err != nil {
		return source, err
	}
	return &observedOAuthTokenSource{base: source, required: h.required, issued: h.issued}, nil
}
func (h *observedOAuthHandler) Authorize(ctx context.Context, request *http.Request, response *http.Response) error {
	h.authorizeMu.Lock()
	defer h.authorizeMu.Unlock()
	if h.authorizationError != nil {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return h.authorizationError
	}
	// Multiple transport requests may have observed the old token before an
	// explicit browser flow finished. Retry those with the newly issued token,
	// rather than opening another consent flow or replacing its state.
	source, sourceErr := h.base.TokenSource(ctx)
	if sourceErr != nil {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		if authorizationRequired(sourceErr) {
			h.required(sourceErr)
		}
		h.authorizationError = sourceErr
		return sourceErr
	}
	if sourceErr == nil && source != nil {
		if token, err := source.Token(); err == nil {
			h.issued(token)
			probe := request.Clone(ctx)
			probe.Header = request.Header.Clone()
			token.SetAuthHeader(probe)
			if probe.Header.Get("Authorization") != request.Header.Get("Authorization") {
				if response.Body != nil {
					_ = response.Body.Close()
				}
				return nil
			}
		} else if authorizationRequired(err) {
			if response.Body != nil {
				_ = response.Body.Close()
			}
			h.required(err)
			h.authorizationError = err
			return err
		}
	}
	err := h.base.Authorize(ctx, request, response)
	h.authorizationError = err
	if authorizationRequired(err) {
		h.required(err)
	}
	return err
}

type observedOAuthTokenSource struct {
	base     oauth2.TokenSource
	required func(error)
	issued   func(*oauth2.Token)
}

func (s *observedOAuthTokenSource) Token() (*oauth2.Token, error) {
	token, err := s.base.Token()
	if err == nil {
		s.issued(token)
	}
	if authorizationRequired(err) {
		s.required(err)
	}
	return token, err
}

func (m *Manager) connectionOptions(entry *serverEntry) (ConnectionOptions, error) {
	options := m.options.Connection
	options.Secrets = append([]string(nil), options.Secrets...)
	m.mu.RLock()
	for secret := range m.secrets {
		options.Secrets = append(options.Secrets, secret)
	}
	m.mu.RUnlock()
	if entry.config.OAuth == nil {
		return options, nil
	}
	if m.oauthStoreError != nil {
		return options, m.oauthStoreError
	}
	if m.oauthStore == nil {
		return options, authorizationFailure("missing", "Connect this MCP server with OAuth")
	}
	grant, err := m.oauthStore.connectionGrant(entry.credentialKey(), entry.oauthBinding)
	if err != nil {
		return options, err
	}
	m.rememberOAuthSecrets(grant)
	options.Secrets = append(options.Secrets, grant.Token.AccessToken, grant.Token.RefreshToken, grant.Config.ClientSecret)
	options.OAuth = &storedOAuthHandler{source: m.oauthStore.tokenSource(m.ctx, entry.credentialKey(), grant, m.oauthHTTPClient(entry.config.OAuth))}
	return options, nil
}

func (m *Manager) rememberOAuthSecrets(grant oauthGrant) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, value := range []string{grant.Token.AccessToken, grant.Token.RefreshToken, grant.Config.ClientSecret} {
		if value != "" {
			m.secrets[value] = true
		}
	}
}

func validOAuthURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(raw) > 8192 {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
}

func (m *Manager) authorizationView(attempt *oauthAttempt, view mcpconfig.OAuthAuthorization) {
	if view.Phase == "" || view.State == "pending" || view.State == "failed" || view.State == "expired" || view.State == "cancelled" || view.State == "authorized" {
		switch view.State {
		case "preparing":
			view.Phase = "starting"
		case "pending":
			view.Phase = "waiting-browser"
		case "failed", "expired":
			view.Phase = "failed"
		case "cancelled":
			view.Phase = "cancelled"
		case "authorized":
			view.Phase = "authorized"
		}
	}
	m.mu.Lock()
	attempt.view = view
	owner := m.authorizations[attempt.entry.name]
	currentView := attempt.entry.status.Authorization
	if m.entries[attempt.entry.name] == attempt.entry && (owner == attempt || (owner == nil && (currentView == nil || currentView.ID == view.ID))) {
		copy := view
		attempt.entry.status.Authorization = &copy
	}
	m.mu.Unlock()
	m.changed()
}

// Authorize starts one explicit browser flow and returns once its authorization
// URL is ready. Waiting for the browser happens in Host memory, never Redis.
func (m *Manager) Authorize(ctx context.Context, name, redirect string) (mcpconfig.OAuthAuthorization, error) {
	attempt, fresh, err := m.beginAuthorization(ctx, name, redirect)
	if err != nil {
		return mcpconfig.OAuthAuthorization{}, err
	}
	if !fresh {
		m.mu.RLock()
		view := attempt.view
		m.mu.RUnlock()
		return view, nil
	}
	select {
	case <-attempt.ready:
		m.mu.RLock()
		view := attempt.view
		m.mu.RUnlock()
		return view, nil
	case <-attempt.done:
		m.mu.RLock()
		view, err := attempt.view, attempt.err
		m.mu.RUnlock()
		return view, err
	case <-ctx.Done():
		attempt.cancel()
		return mcpconfig.OAuthAuthorization{}, ctx.Err()
	}
}

func (m *Manager) beginAuthorization(ctx context.Context, name, redirect string) (*oauthAttempt, bool, error) {
	if err := validateOAuthCallback(redirect); err != nil {
		return nil, false, err
	}
	leave, err := m.enter(ctx)
	if err != nil {
		return nil, false, err
	}
	defer leave()
	m.mu.RLock()
	previous := m.authorizations[name]
	var activeView mcpconfig.OAuthAuthorization
	if previous != nil {
		activeView = previous.view
	}
	m.mu.RUnlock()
	if previous != nil {
		switch activeView.Phase {
		case "starting", "waiting-browser", "exchanging", "committing":
			// A second start observes the same active authorization; only an
			// explicit cancellation or owner retirement withdraws its state.
			return previous, false, nil
		}
	}
	if err := m.cancelOAuthAttempt(ctx, name); err != nil {
		return nil, false, err
	}
	m.mu.RLock()
	entry := m.entries[name]
	cleanupFailed := entry != nil && entry.cleanupFailure != nil
	m.mu.RUnlock()
	if cleanupFailed {
		return nil, false, fail("MCP_CLEANUP_INCOMPLETE", "MCP authorization cleanup failed; restart the Host before authorizing again")
	}
	if entry == nil || entry.config.OAuth == nil {
		return nil, false, fail("MCP_NOT_FOUND", "OAuth MCP server is not configured")
	}
	if len(entry.status.Missing) > 0 {
		return nil, false, fail("MCP_CREDENTIAL_REQUIRED", "Configure the missing MCP credentials first")
	}
	if m.oauthStoreError != nil {
		return nil, false, m.oauthStoreError
	}
	if m.oauthStore == nil {
		return nil, false, fail("MCP_OAUTH_STORE_UNAVAILABLE", "Host OAuth credential storage is not configured")
	}
	lifetime, cancelCause := context.WithCancelCause(m.ctx)
	cancel := func() { cancelCause(context.Canceled) }
	budget := oauthAuthorizationTimeout(entry.config.OAuth)
	attempt := &oauthAttempt{entry: entry, ctx: lifetime, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}, 1), answer: make(chan oauthCallbackResult, 1), view: mcpconfig.OAuthAuthorization{ID: uuid.NewString(), Phase: "starting", Name: name, State: "preparing", ExpiresAt: time.Now().Add(budget).UnixMilli()}}
	m.mu.Lock()
	attempt.deadline = time.AfterFunc(budget, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !attempt.committing {
			cancelCause(context.DeadlineExceeded)
		}
	})
	m.authorizations[name] = attempt
	m.mu.Unlock()
	m.authorizationView(attempt, attempt.view)
	go m.runAuthorization(attempt, redirect)
	return attempt, true, nil
}

func (m *Manager) runAuthorization(attempt *oauthAttempt, redirect string) {
	entry := attempt.entry
	client, registration := m.registrationClient(entry, redirect)
	var operationErr error
	var cleanupErr error
	defer func() {
		cause := context.Cause(attempt.ctx)
		var rejection *oauth2.RetrieveError
		if errors.As(operationErr, &rejection) && rejection.ErrorCode == "invalid_client" {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(attempt.ctx), CloseTimeout)
			operationErr = errors.Join(operationErr, registration.invalidate(cleanup))
			cancel()
		}
		attempt.cancel()
		// Registration secrets can be learned after the temporary Connection
		// was created; redact once more with the owner's complete secret set.
		var publicErr error
		if operationErr != nil {
			publicErr = &Error{Code: "MCP_OAUTH_FAILED", Message: m.redact(operationErr.Error()), Cause: operationErr}
		}
		m.mu.Lock()
		if attempt.deadline != nil {
			attempt.deadline.Stop()
		}
		// Cancellation cannot retract a credential commit already admitted.
		// A cleanup failure remains a failure independently of that commit.
		// Authorization is a credential outcome. Subsequent MCP connection
		// failures are reported by the supervisor, not as a retracted grant.
		if attempt.committed && cleanupErr == nil {
			operationErr = nil
			publicErr = nil
		}
		attempt.err = publicErr
		view := attempt.view
		committed := attempt.committed
		m.mu.Unlock()
		if operationErr != nil {
			view.State = "failed"
			if cleanupErr == nil && errors.Is(cause, context.DeadlineExceeded) {
				view.State = "expired"
			} else if cleanupErr == nil && errors.Is(cause, context.Canceled) {
				view.State = "cancelled"
			}
			view.URL = ""
			view.Error = m.redact(operationErr.Error())
			m.authorizationView(attempt, view)
		} else if committed {
			view.URL, view.State = "", "authorized"
			m.authorizationView(attempt, view)
		}
		close(attempt.done)
	}()
	acquire := func(ctx context.Context, commit AuthorizationCommit, admit func() error) error {
		if err := ctx.Err(); err != nil {
			attempt.cancel()
			return err
		}
		stop := context.AfterFunc(ctx, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if !attempt.committing {
				attempt.cancel()
			}
		})
		defer stop()
		options := m.options.Connection
		options.StartupTimeout = oauthAuthorizationTimeout(entry.config.OAuth)
		previousCleanup := options.connectCleanup
		options.connectCleanup = func(err error) {
			if previousCleanup != nil {
				previousCleanup(err)
			}
			cleanupErr = err
			if err != nil {
				m.retainCleanup(entry, mcpconfig.CloseResult{CloseConfirmed: false, CleanupError: m.redact(err.Error())})
			}
		}
		oauthConfig := entry.config.OAuth
		var resource string
		handlerConfig := &auth.AuthorizationCodeHandlerConfig{RedirectURL: redirect, Client: client,
			AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
				m.mu.RLock()
				committed := attempt.committed
				m.mu.RUnlock()
				if committed {
					// The explicit flow ends at credential commit. Subsequent scope
					// challenges belong to the managed connection and a new explicit
					// authorization, not an unbounded second browser wait here.
					return nil, authorizationFailure("scope", "MCP server requires another explicit OAuth authorization")
				}
				if !validOAuthURL(args.URL) {
					return nil, fail("MCP_INVALID_AUTHORIZATION_URL", "OAuth provider returned an invalid authorization URL")
				}
				if err := registration.admit(ctx, args.URL); err != nil {
					return nil, err
				}
				parsed, _ := url.Parse(args.URL)
				resource = parsed.Query().Get("resource")
				state := parsed.Query().Get("state")
				if state == "" || len(state) > 512 {
					return nil, fail("MCP_INVALID_AUTHORIZATION_URL", "OAuth provider returned an invalid state")
				}
				issuer, issuerRequired := registration.callbackIssuer()
				m.mu.Lock()
				attempt.state, attempt.consumed = state, false
				attempt.issuer, attempt.issuerRequired = issuer, issuerRequired
				view := attempt.view
				m.mu.Unlock()
				view.URL, view.State = args.URL, "pending"
				m.authorizationView(attempt, view)
				select {
				case attempt.ready <- struct{}{}:
				default:
				}
				select {
				case result := <-attempt.answer:
					if result.value != nil && issuer == "" {
						result.value.Iss = ""
					}
					return result.value, result.err
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
			NewTokenSource: func(_ context.Context, config *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
				m.mu.Lock()
				if err := attempt.ctx.Err(); err != nil {
					m.mu.Unlock()
					return nil, err
				}
				current := m.entries[entry.name] == entry
				if !current {
					m.mu.Unlock()
					return nil, fail("MCP_AUTHORIZATION_REQUIRED", "MCP configuration changed during authorization")
				}
				if admit != nil {
					if err := admit(); err != nil {
						m.mu.Unlock()
						return nil, err
					}
				}
				attempt.committing = true
				if attempt.deadline != nil {
					attempt.deadline.Stop()
				}
				view := attempt.view
				m.mu.Unlock()
				scope, scopePresent := token.Extra("scope").(string)
				metadata, rawMetadata := registration.authorizationMetadata()
				grant := oauthGrant{ID: uuid.NewString(), Binding: entry.oauthBinding, ServerURL: entry.config.URL, Issuer: metadata.Issuer,
					ClientKey: m.oauthClientKey(entry, metadata.Issuer, redirect), ConfiguredScope: oauthConfig.RequestedScope(), Metadata: rawMetadata,
					Config: *config, Token: *token, Resource: resource, Scope: scope, ScopePresent: scopePresent}
				if oauthConfig.ClientID != "" {
					grant.ConfiguredClientID = &oauthConfig.ClientID
				}
				view.Phase = "committing"
				m.authorizationView(attempt, view)
				m.rememberOAuthSecrets(grant)
				// Once admitted, publication finishes even if cancellation or Host
				// shutdown arrives. The cancelling owner drains attempt.done before
				// deleting credentials or replacing this configuration.
				var commitErr error
				if commit != nil {
					record, err := oauthRecord("mcp-oauth-grant", grant)
					if err != nil {
						return nil, err
					}
					commitErr = commit(record)
				} else {
					// Low-level Go-only manager fixtures have no Cordis service.
					commitErr = m.oauthStore.put(context.WithoutCancel(attempt.ctx), entry.credentialKey(), grant)
				}
				if commitErr != nil {
					return nil, commitErr
				}
				m.mu.Lock()
				attempt.committed = true
				m.mu.Unlock()
				return m.oauthStore.tokenSource(m.ctx, entry.credentialKey(), grant, m.oauthHTTPClient(entry.config.OAuth)), nil
			},
			// The TypeScript SDK accepts a matching iss even when the server did
			// not advertise it. Go SDK's default is stricter; use the same table.
			AcceptUnadvertisedIss: true,
		}
		var challengeScope []string
		var scopeMu sync.Mutex
		handlerConfig.ScopeFilter = func([]string) []string {
			if scope := oauthConfig.RequestedScope(); scope != nil {
				return strings.Fields(*scope)
			}
			scopeMu.Lock()
			defer scopeMu.Unlock()
			return append([]string(nil), challengeScope...)
		}
		if oauthConfig.ClientID != "" {
			credentials := &oauthex.ClientCredentials{ClientID: oauthConfig.ClientID}
			if oauthConfig.ClientSecretRef != "" || oauthConfig.ClientSecret != "" {
				secret := oauthConfig.ClientSecret
				if secret == "" {
					m.mu.RLock()
					secret = m.desired.Credentials[oauthConfig.ClientSecretRef]
					m.mu.RUnlock()
				}
				if secret == "" {
					operationErr = fail("MCP_CREDENTIAL_REQUIRED", "OAuth client secret is missing")
					return operationErr
				}
				credentials.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: secret}
			}
			handlerConfig.PreregisteredClient = credentials
		} else {
			handlerConfig.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{RedirectURIs: []string{redirect}, ClientName: "Go Bot", GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none"}}
			if scope := oauthConfig.RequestedScope(); scope != nil {
				handlerConfig.DynamicClientRegistrationConfig.Metadata.Scope = *scope
			}
		}
		registration.preregistered = handlerConfig.PreregisteredClient
		handler, err := auth.NewAuthorizationCodeHandler(handlerConfig)
		if err != nil {
			operationErr = err
			return operationErr
		}
		options.OAuth = challengeScopeOAuthHandler{base: handler, observe: func(response *http.Response) {
			var scopes []string
			if challenges, err := oauthex.ParseWWWAuthenticate(response.Header.Values("WWW-Authenticate")); err == nil {
				for _, challenge := range challenges {
					if challenge.Scheme == "bearer" {
						scopes = strings.Fields(challenge.Params["scope"])
						break
					}
				}
			}
			scopeMu.Lock()
			challengeScope = scopes
			scopeMu.Unlock()
		}}
		connection, err := Connect(attempt.ctx, entry.name, entry.config, options)
		if connection != nil {
			cleanupErr = connection.Close()
			if cleanupErr != nil {
				m.retainCleanup(entry, mcpconfig.CloseResult{CloseConfirmed: transportClosed(connection), CleanupError: m.redact(cleanupErr.Error())})
			}
		}
		if cleanupErr != nil {
			operationErr = cleanupErr
			return operationErr
		}
		m.mu.RLock()
		committed := attempt.committed
		m.mu.RUnlock()
		if !committed {
			if err == nil {
				err = fail("MCP_OAUTH_NOT_REQUIRED", "This endpoint did not request OAuth; check its configuration")
			}
			operationErr = err
			return operationErr
		}
		return nil
	}
	if service := m.options.Authorization; service != nil {
		key := oauthGrantKey(entry.credentialKey())
		dispose, err := service.Register(authservice.Flow{Key: key, Label: entry.name, Methods: []authservice.Method{{ID: "oauth", Label: "OAuth"}}, Run: func(session *authservice.Session) error {
			return acquire(session.Context, session.Commit, session.AdmitCommit)
		}})
		if err != nil {
			operationErr = err
		} else {
			outcome, err := service.BeginDrained(attempt.ctx, key, "oauth", authservice.Interaction{})
			dispose()
			operationErr = err
			if err == nil && outcome.Status == "cancelled" {
				operationErr = context.Canceled
			}
		}
	} else {
		operationErr = acquire(attempt.ctx, nil, nil)
	}

	if operationErr != nil {
		return
	}
	if err := attempt.ctx.Err(); err != nil {
		operationErr = err
		return
	}
	// The flow no longer needs cancellation through entry replacement. Remove
	// it before Restart so its own cleanup cannot wait for its own completion.
	m.mu.Lock()
	if m.authorizations[entry.name] == attempt {
		delete(m.authorizations, entry.name)
	}
	view := attempt.view
	view.URL, view.State = "", "connecting"
	attempt.view = view
	if m.entries[entry.name] == entry {
		copy := view
		entry.status.Authorization = &copy
	}
	m.mu.Unlock()
	m.changed()
	if entry.config.IsEnabled() {
		if err := m.activateAuthorized(attempt.ctx, attempt); err != nil {
			m.mu.RLock()
			retained := attempt.entry.cleanupFailure != nil
			m.mu.RUnlock()
			if retained {
				cleanupErr = err
			}
			operationErr = err
			return
		}
	}
	m.mu.RLock()
	view = attempt.view
	m.mu.RUnlock()
	view.URL, view.State = "", "authorized"
	m.authorizationView(attempt, view)
}

func (m *Manager) activateAuthorized(ctx context.Context, attempt *oauthAttempt) error {
	leave, err := m.enter(ctx)
	if err != nil {
		return err
	}
	defer leave()
	return m.activateAuthorizedLocked(ctx, attempt)
}

// The caller owns the manager gate. This is also used after a cancellation
// drained an admitted commit, without re-entering that same gate.
func (m *Manager) activateAuthorizedLocked(ctx context.Context, attempt *oauthAttempt) error {
	entry := attempt.entry
	m.mu.RLock()
	current := m.entries[entry.name] == entry
	m.mu.RUnlock()
	if !current {
		return fail("MCP_AUTHORIZATION_REQUIRED", "MCP configuration changed during authorization")
	}
	if err := m.remove(ctx, entry.name, entry.owner); err != nil {
		return err
	}
	err := m.upsert(ctx, entry.name, entry.owner, entry.config, entry.status.Missing)
	m.mu.Lock()
	if next := m.entries[entry.name]; next != nil {
		attempt.entry = next
		copy := attempt.view
		next.status.Authorization = &copy
	}
	m.mu.Unlock()
	m.changed()
	return err
}

func (m *Manager) acceptAuthorization(input mcpconfig.OAuthCallback) (*oauthAttempt, error) {
	if input.State == "" || len(input.State) > 512 || len(input.Code) > 8192 || len(input.Issuer) > 8192 || len(input.Error) > 256 || (input.Code == "") == (input.Error == "") {
		return nil, fail("MCP_INVALID_AUTHORIZATION", "Invalid OAuth callback")
	}
	m.mu.Lock()
	var attempt *oauthAttempt
	for _, candidate := range m.authorizations {
		if subtle.ConstantTimeCompare([]byte(candidate.state), []byte(input.State)) == 1 && !candidate.consumed && candidate.ctx.Err() == nil && candidate.view.Phase == "waiting-browser" {
			attempt = candidate
			break
		}
	}
	if attempt == nil {
		m.mu.Unlock()
		return nil, fail("MCP_INVALID_AUTHORIZATION", "OAuth state is invalid, expired or already used")
	}
	// Upstream validates the issuer before consuming the callback, including
	// error callbacks. A malformed callback cannot withdraw a valid browser flow.
	if attempt.issuer != "" && ((input.Issuer == "" && attempt.issuerRequired) || (input.Issuer != "" && input.Issuer != attempt.issuer)) {
		m.mu.Unlock()
		return nil, fail("MCP_INVALID_AUTHORIZATION", "OAuth callback issuer does not match the authorization server")
	}
	attempt.consumed = true
	view := attempt.view
	m.mu.Unlock()
	if input.Error == "access_denied" {
		attempt.cancel()
	} else if input.Error != "" {
		var err error = fail("MCP_OAUTH_FAILED", "MCP OAuth authorization was rejected")
		if input.Error == "invalid_client" {
			// Preserve the protocol error category for dynamic-client invalidation;
			// never copy callback descriptions or arbitrary error text into logs.
			err = &oauth2.RetrieveError{ErrorCode: "invalid_client", ErrorDescription: "MCP OAuth client registration was rejected"}
		}
		attempt.answer <- oauthCallbackResult{err: err}
	} else {
		view.URL, view.State = "", "connecting"
		view.Phase = "exchanging"
		m.authorizationView(attempt, view)
		m.mu.Lock()
		m.secrets[input.Code] = true
		m.mu.Unlock()
		attempt.answer <- oauthCallbackResult{value: &auth.AuthorizationResult{Code: input.Code, State: input.State, Iss: input.Issuer}}
	}
	return attempt, nil
}

// ReceiveAuthorization acknowledges a valid callback independently of token
// exchange, credential commit and managed reconnection, as upstream's HTTP route
// does. The owning flow and its existing status stream report the final outcome.
func (m *Manager) ReceiveAuthorization(input mcpconfig.OAuthCallback) (mcpconfig.OAuthCallbackReceipt, error) {
	_, err := m.acceptAuthorization(input)
	if err != nil {
		return mcpconfig.OAuthCallbackReceipt{}, err
	}
	state := "received"
	if input.Error != "" {
		state = "rejected"
	}
	return mcpconfig.OAuthCallbackReceipt{State: state}, nil
}

// CompleteAuthorization is the internal synchronous Go/test seam. It admits
// exactly the same callback, then waits for the owning authorization to settle.
func (m *Manager) CompleteAuthorization(ctx context.Context, input mcpconfig.OAuthCallback) (mcpconfig.OAuthAuthorization, error) {
	attempt, err := m.acceptAuthorization(input)
	if err != nil {
		return mcpconfig.OAuthAuthorization{}, err
	}
	select {
	case <-attempt.done:
		m.mu.RLock()
		view, err := attempt.view, attempt.err
		m.mu.RUnlock()
		return view, err
	case <-ctx.Done():
		return mcpconfig.OAuthAuthorization{}, ctx.Err()
	}
}

func (m *Manager) cancelOAuthAttempt(ctx context.Context, name string) error {
	m.mu.RLock()
	attempt := m.authorizations[name]
	m.mu.RUnlock()
	if attempt == nil {
		return nil
	}
	attempt.cancel()
	timer := time.NewTimer(CloseTimeout)
	defer timer.Stop()
	select {
	case <-attempt.done:
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fail("MCP_CLEANUP_TIMEOUT", "OAuth authorization cleanup has not finished")
	}
	m.mu.Lock()
	if m.authorizations[name] == attempt {
		delete(m.authorizations, name)
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) CancelAuthorization(ctx context.Context, name string) (mcpconfig.Status, error) {
	leave, err := m.enter(ctx)
	if err != nil {
		return m.Status(), err
	}
	defer leave()
	err = m.cancelAuthorizationLocked(ctx, name)
	return m.Status(), err
}

func (m *Manager) cancelAuthorizationLocked(ctx context.Context, name string) error {
	m.mu.RLock()
	attempt := m.authorizations[name]
	m.mu.RUnlock()
	if err := m.cancelOAuthAttempt(ctx, name); err != nil {
		return err
	}
	if attempt == nil {
		return nil
	}
	m.mu.RLock()
	activate := attempt.committed && attempt.err == nil && m.entries[name] == attempt.entry && attempt.entry.config.IsEnabled()
	m.mu.RUnlock()
	if activate {
		return m.activateAuthorizedLocked(ctx, attempt)
	}
	return nil
}

// CancelAuthorizationAttempt never cancels a replacement attempt selected by
// another client. The public all-current-attempt HTTP operation remains separate.
func (m *Manager) CancelAuthorizationAttempt(ctx context.Context, name, id string) error {
	leave, err := m.enter(ctx)
	if err != nil {
		return err
	}
	defer leave()
	m.mu.RLock()
	attempt := m.authorizations[name]
	selected := attempt != nil && id != "" && attempt.view.ID == id
	m.mu.RUnlock()
	if !selected {
		return nil
	}
	return m.cancelAuthorizationLocked(ctx, name)
}

func (m *Manager) DisconnectAuthorization(ctx context.Context, name string) (mcpconfig.Status, error) {
	leave, err := m.enter(ctx)
	if err != nil {
		return m.Status(), err
	}
	defer leave()
	m.mu.RLock()
	entry := m.entries[name]
	m.mu.RUnlock()
	if entry == nil || entry.config.OAuth == nil {
		return m.Status(), fail("MCP_NOT_FOUND", "OAuth MCP server is not configured")
	}
	if err := m.cancelOAuthAttempt(ctx, name); err != nil {
		return m.Status(), err
	}
	if err := m.remove(ctx, name, entry.owner); err != nil {
		return m.Status(), err
	}
	var deleteErr error
	if m.oauthStore != nil {
		deleteErr = m.oauthStore.remove(ctx, entry.credentialKey())
	}
	// Even a failed credential deletion must leave the configured namespace
	// managed, so inspection and a later explicit retry remain possible.
	err = m.upsert(ctx, name, entry.owner, entry.config, entry.status.Missing)
	return m.Status(), errors.Join(deleteErr, err)
}
