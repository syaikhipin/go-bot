package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"go-bot/internal/credentials"
	mcpconfig "go-bot/mcp"
	"golang.org/x/oauth2"
)

func TestMCPUsesSharedGoCredentialsAndLeavesOtherPluginRecordsIntact(t *testing.T) {
	f := oauthServiceFixture(t)
	path := filepath.Join(t.TempDir(), credentials.Filename)
	store, err := credentials.Open(path, credentials.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.ModifyRecord(t.Context(), "calendar/personal", func(*credentials.Record) (*credentials.Record, error) {
		return &credentials.Record{Kind: "api-key"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir(), Credentials: store})
	defer manager.Close()
	settings := f.settings("")
	server := settings.Servers["oauth"]
	server.OAuth.CredentialKey = "mail/personal"
	settings.Servers["oauth"] = server
	if _, err = manager.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	view, err := manager.Authorize(t.Context(), "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.CompleteAuthorization(t.Context(), visitOAuthAuthorization(t, view)); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ListRecords()
	if err != nil || len(entries) != 3 {
		t.Fatalf("shared records: %+v %v", entries, err)
	}
	grant, err := store.ReadRecord("mail/personal")
	if err != nil || grant == nil || grant.Kind != "grant" {
		t.Fatal("MCP grant bypassed the shared service", err)
	}
	var authorized oauthGrant
	if err := decodeOAuthRecord(grant, "mcp-oauth-grant", &authorized); err != nil || authorized.Resource != f.resource.URL+"/mcp" || authorized.Scope != "read offline_access" {
		t.Fatal("SDK authorization lost the selected resource or returned scope", err)
	}
	for _, entry := range entries {
		if entry.Key != "calendar/personal" && entry.Key != "mail/personal" && !strings.HasPrefix(entry.Key, "mail/personal-client-") {
			t.Fatalf("client registration lost owning namespace: %s", entry.Key)
		}
	}
	if _, err = manager.DisconnectAuthorization(t.Context(), "oauth"); err != nil {
		t.Fatal(err)
	}
	if record, _ := store.ReadRecord("mail/personal"); record != nil {
		t.Fatal("sign-out retained grant")
	}
	entries, err = store.ListRecords()
	if err != nil || len(entries) != 2 {
		t.Fatalf("sign-out removed unrelated or retained-client records: %+v %v", entries, err)
	}
	if err = manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ListRecords(); err != nil {
		t.Fatal("MCP closed a Host-owned credential service", err)
	}
}

func fixtureOAuthGrant() oauthGrant {
	grant := oauthGrant{ID: uuid.NewString(), ServerURL: "https://resource.example/mcp", Issuer: "https://auth.example", ClientKey: "mcp-client/fixture-client", Config: oauth2.Config{ClientID: "fixture-client", ClientSecret: "private-client-secret", Endpoint: oauth2.Endpoint{AuthURL: "https://auth.example/authorize", TokenURL: "https://auth.example/token"}}, Token: oauth2.Token{AccessToken: "private-access", RefreshToken: "private-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)}}
	grant.Binding = oauthIdentityBinding(grant.ServerURL, nil, nil)
	grant.Metadata = json.RawMessage(`{"issuer":"https://auth.example","authorization_endpoint":"https://auth.example/authorize","token_endpoint":"https://auth.example/token","response_types_supported":["code"]}`)
	return grant
}

func saveFixtureOAuthClient(t *testing.T, store *oauthGrantStore, grant *oauthGrant) {
	t.Helper()
	grant.Metadata, _ = json.Marshal(map[string]any{"issuer": grant.Issuer, "authorization_endpoint": grant.Config.Endpoint.AuthURL, "token_endpoint": grant.Config.Endpoint.TokenURL, "response_types_supported": []string{"code"}})
	method := "client_secret_basic"
	if grant.Config.Endpoint.AuthStyle == oauth2.AuthStyleInParams {
		method = "client_secret_post"
	}
	if err := store.saveClient(t.Context(), grant.ClientKey, oauthClientRegistration{Issuer: grant.Issuer, Response: &oauthex.ClientRegistrationResponse{ClientID: grant.Config.ClientID, ClientSecret: grant.Config.ClientSecret, ClientRegistrationMetadata: oauthex.ClientRegistrationMetadata{TokenEndpointAuthMethod: method}}}); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthGrantPersistsPrivatelyAcrossStoreReopenWithoutCrossingBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dsh", "mcp", "credentials.json")
	store, err := newOAuthGrantStore(path)
	if err != nil {
		t.Fatal(err)
	}
	grant := fixtureOAuthGrant()
	if err := store.put(t.Context(), "mail", grant); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credential permissions: %v %v", info, err)
	}
	reopened, err := newOAuthGrantStore(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, found, err := reopened.get("mail", grant.Binding)
	if err != nil || !found || !sameOAuthToken(loaded.Token, grant.Token) || loaded.Config.ClientSecret != "" {
		t.Fatalf("grant did not survive restart: found=%v err=%v", found, err)
	}
	if _, found, err := reopened.get("mail", "another-resource-or-client"); err != nil || found {
		t.Fatal("grant crossed its configured binding", err)
	}
	if err := reopened.remove(t.Context(), "mail"); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := newOAuthGrantStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := afterDelete.get("mail", grant.Binding); err != nil || found {
		t.Fatal("deleted grant remained", err)
	}
}

func TestOAuthRefreshRotatesAndPersistsOnceForConcurrentRequests(t *testing.T) {
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.FormValue("grant_type") != "refresh_token" || r.FormValue("refresh_token") != "private-refresh" {
			http.Error(w, "bad refresh request", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer remote.Close()
	store, _ := newOAuthGrantStore(filepath.Join(t.TempDir(), "credentials.json"))
	grant := fixtureOAuthGrant()
	grant.Token.Expiry = time.Now().Add(-time.Hour)
	grant.Config.Endpoint.TokenURL = remote.URL
	grant.Config.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	saveFixtureOAuthClient(t, store, &grant)
	if err := store.put(t.Context(), "mail", grant); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 12 {
		// Independent managed/probe transports share only the Go provider,
		// not one oauth2.ReuseTokenSource mutex.
		source := store.tokenSource(t.Context(), "mail", grant, remote.Client())
		group.Add(1)
		go func() {
			defer group.Done()
			token, err := source.Token()
			if err != nil || token.AccessToken != "rotated-access" {
				t.Errorf("refresh token=%v err=%v", token, err)
			}
		}()
	}
	group.Wait()
	if requests.Load() != 1 {
		t.Fatalf("refresh requests=%d; want one", requests.Load())
	}
	loaded, found, err := store.get("mail", grant.Binding)
	if err != nil || !found || loaded.Token.AccessToken != "rotated-access" || loaded.Token.RefreshToken != "rotated-refresh" {
		t.Fatalf("rotated grant was not persisted: found=%v err=%v", found, err)
	}
}

func TestOAuthConcurrentRejectedRequestsReuseTheSharedCommittedRotation(t *testing.T) {
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.FormValue("refresh_token") != "private-refresh" {
			http.Error(w, "rotated refresh token was reused", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "resource-retry-access", "refresh_token": "resource-retry-refresh", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer remote.Close()
	store, err := newOAuthGrantStore(filepath.Join(t.TempDir(), credentials.Filename))
	if err != nil {
		t.Fatal(err)
	}
	defer store.credentials.Close()
	grant := fixtureOAuthGrant()
	grant.Config.Endpoint.TokenURL = remote.URL
	grant.Config.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	saveFixtureOAuthClient(t, store, &grant)
	if err := store.put(t.Context(), "mail", grant); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 12 {
		source := store.tokenSource(t.Context(), "mail", grant, remote.Client())
		group.Go(func() {
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1/mcp", nil)
			request.Header.Set("Authorization", "Bearer "+grant.Token.AccessToken)
			if err := (&storedOAuthHandler{source: source}).Authorize(t.Context(), request, &http.Response{StatusCode: http.StatusUnauthorized}); err != nil {
				t.Error("Shared revoked-token refresh failed", err)
				return
			}
			if token, err := source.Token(); err != nil || token.AccessToken != "resource-retry-access" {
				t.Error("Did not reuse the committed token", err)
			}
		})
	}
	group.Wait()
	if requests.Load() != 1 {
		t.Fatalf("Independent requests performed %d rotations; want one", requests.Load())
	}
}

func TestOAuthRefreshPreservesAuthorizedResourceAndScopeAcrossReopen(t *testing.T) {
	const resource = "https://mcp.example/reports?tenant=team"
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sequence := requests.Add(1)
		if r.Method != http.MethodPost || r.FormValue("grant_type") != "refresh_token" || r.FormValue("resource") != resource || r.FormValue("client_id") != "fixture-client" || r.FormValue("client_secret") != "private-client-secret" {
			http.Error(w, "refresh lost resource or client authentication", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		value := map[string]any{"access_token": fmt.Sprintf("access-%d", sequence), "token_type": "Bearer"}
		if sequence == 2 {
			value["scope"] = "read write"
			value["expires_in"] = 3600
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer remote.Close()
	path := filepath.Join(t.TempDir(), credentials.Filename)
	grant := fixtureOAuthGrant()
	grant.Resource, grant.Scope = resource, "read offline_access"
	grant.Config.Endpoint.TokenURL = remote.URL
	grant.Config.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	store, err := newOAuthGrantStore(path)
	if err != nil {
		t.Fatal(err)
	}
	saveFixtureOAuthClient(t, store, &grant)
	if err := store.put(t.Context(), "mail", grant); err != nil {
		t.Fatal(err)
	}
	if err := store.credentials.Close(); err != nil {
		t.Fatal(err)
	}
	// Actual serialization/reopen is required: oauth2.Token drops its Extra
	// fields from JSON, so an in-memory token alone cannot preserve scope.
	store, err = newOAuthGrantStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.credentials.Close()
	for sequence, expectedScope := range []string{"read offline_access", "read write", "read write"} {
		grant, err = store.refresh(t.Context(), "mail", grant.Binding, grant.ID, grant.Token.AccessToken, true, remote.Client())
		if err != nil {
			t.Fatal(err)
		}
		if grant.Resource != resource || grant.Scope != expectedScope || grant.Token.RefreshToken != "private-refresh" {
			t.Fatalf("refresh %d lost resource, scope or retained refresh token", sequence+1)
		}
		if (sequence == 1) == grant.Token.Expiry.IsZero() {
			t.Fatalf("refresh %d did not replace the prior expiry", sequence+1)
		}
	}
	if requests.Load() != 3 {
		t.Fatalf("unexpected refresh attempts: %d", requests.Load())
	}
	loaded, found, err := store.get("mail", grant.Binding)
	if err != nil || !found || loaded.Scope != "read write" || loaded.Resource != resource {
		t.Fatal("refreshed resource and scope were not persisted", err)
	}
}

func TestLateOAuthSourceCannotResurrectOrOverwriteAnotherAuthorization(t *testing.T) {
	for _, operation := range []string{"sign-out", "new-authorization", "new-authorization-same-token", "binding-change"} {
		t.Run(operation, func(t *testing.T) {
			store, _ := newOAuthGrantStore(filepath.Join(t.TempDir(), "credentials.json"))
			grant := fixtureOAuthGrant()
			if err := store.put(t.Context(), "mail", grant); err != nil {
				t.Fatal(err)
			}
			source := store.tokenSource(t.Context(), "mail", grant, nil)
			if operation == "sign-out" {
				if err := store.remove(t.Context(), "mail"); err != nil {
					t.Fatal(err)
				}
			} else {
				current := grant
				current.ID = uuid.NewString()
				if operation != "new-authorization-same-token" {
					current.Token.AccessToken = "new-authorization-token"
				}
				if operation == "binding-change" {
					current.ServerURL += "/other"
					current.Binding = oauthIdentityBinding(current.ServerURL, nil, nil)
				}
				if err := store.put(t.Context(), "mail", current); err != nil {
					t.Fatal(err)
				}
			}
			_, err := source.Token()
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "MCP_AUTHORIZATION_REQUIRED" {
				t.Fatalf("stale source remained usable: %v", err)
			}
			binding := grant.Binding
			if operation == "binding-change" {
				binding = oauthIdentityBinding(grant.ServerURL+"/other", nil, nil)
			}
			loaded, found, err := store.get("mail", binding)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "sign-out" {
				if found {
					t.Fatal("late source resurrected signed-out grant")
				}
			} else if !found || (operation != "new-authorization-same-token" && loaded.Token.AccessToken != "new-authorization-token") {
				t.Fatal("late source replaced the new authorization")
			}
		})
	}
}

func TestOAuthGrantConcurrentWritesAndCancelledSaveKeepCompleteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	store, _ := newOAuthGrantStore(path)
	var group sync.WaitGroup
	for index := range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := store.put(t.Context(), fmt.Sprint(index), fixtureOAuthGrant()); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	reopened, err := newOAuthGrantStore(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := reopened.credentials.ListRecords()
	if err != nil || len(entries) != 12 {
		t.Fatalf("concurrent grant writes lost shared records: %v %v", entries, err)
	}
	for index := range 12 {
		if _, found, err := reopened.get(fmt.Sprint(index), fixtureOAuthGrant().Binding); err != nil || !found {
			t.Fatalf("grant %d was not durable: %v", index, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.put(ctx, "cancelled", fixtureOAuthGrant()); err == nil {
		t.Fatal("cancelled credential write succeeded")
	}
	if _, found, err := store.get("cancelled", fixtureOAuthGrant().Binding); err != nil || found {
		t.Fatal("cancelled write was published", err)
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".credential-write-*"))
	if err != nil || len(files) != 0 {
		t.Fatal("temporary OAuth credential files remained", files, err)
	}
}
