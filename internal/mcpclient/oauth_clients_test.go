package mcpclient

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"go-bot/internal/credentials"
	mcpconfig "go-bot/mcp"
)

func TestOAuthClientSaveDoesNotOverwriteAnotherCredentialKind(t *testing.T) {
	store, err := newOAuthGrantStore(filepath.Join(t.TempDir(), credentials.Filename))
	if err != nil {
		t.Fatal(err)
	}
	defer store.credentials.Close()
	key := "mcp-client/mail-client-test"
	if _, err := store.credentials.ModifyRecord(t.Context(), key, func(*credentials.Record) (*credentials.Record, error) {
		return &credentials.Record{Kind: "api-key"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	registration := oauthClientRegistration{Issuer: "https://issuer.example", Response: &oauthex.ClientRegistrationResponse{ClientID: "fixture"}}
	if err := store.saveClient(t.Context(), key, registration); err == nil {
		t.Fatal("incompatible credential record overwritten")
	}
	if record, err := store.credentials.ReadRecord(key); err != nil || record == nil || record.Kind != "api-key" {
		t.Fatal("original record was lost", err)
	}
}

func TestOAuthRegistrationSurvivesCancellationRestartAndSignOut(t *testing.T) {
	t.Run("public", func(t *testing.T) { testOAuthRegistrationLifecycle(t, false) })
	t.Run("confidential", func(t *testing.T) { testOAuthRegistrationLifecycle(t, true) })
}

func testOAuthRegistrationLifecycle(t *testing.T, confidential bool) {
	t.Helper()
	f := oauthServiceFixture(t)
	f.dynamicConfidential.Store(confidential)
	cwd := t.TempDir()
	manager := NewManager(t.Context(), ManagerOptions{CWD: cwd})
	t.Cleanup(func() { _ = manager.Close() })
	settings := f.settings("")
	if _, err := manager.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	redirect := "http://127.0.0.1:9090" + mcpconfig.OAuthCallbackPath + "alice"
	view, err := manager.Authorize(ctx, "oauth", redirect)
	if err != nil || view.State != "pending" || f.registrations.Load() != 1 {
		t.Fatalf("initial dynamic registration: %+v, %v, calls=%d", view, err, f.registrations.Load())
	}
	if _, err := manager.CancelAuthorization(ctx, "oauth"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := manager.oauthStore.get("oauth", manager.entries["oauth"].oauthBinding); err != nil || found {
		t.Fatal("cancelled consent persisted a grant", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	manager = NewManager(t.Context(), ManagerOptions{CWD: cwd})
	if _, err := manager.Apply(ctx, settings); err != nil {
		t.Fatal(err)
	}
	view, err = manager.Authorize(ctx, "oauth", redirect)
	if err != nil || f.registrations.Load() != 1 {
		t.Fatalf("restart registered a second client before consent: %v, calls=%d", err, f.registrations.Load())
	}
	if _, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.DisconnectAuthorization(ctx, "oauth"); err != nil {
		t.Fatal(err)
	}
	if status := manager.Status().Servers[0]; status.State != "waiting-authorization" {
		t.Fatalf("sign-out did not withdraw authorized connection: %+v", status)
	}
	view, err = manager.Authorize(ctx, "oauth", redirect)
	if err != nil || f.registrations.Load() != 1 {
		t.Fatalf("sign-out discarded registration: %v, calls=%d", err, f.registrations.Load())
	}
	if _, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view)); err != nil {
		t.Fatal(err)
	}
	if status := manager.Status().Servers[0]; status.State != "connected" {
		t.Fatalf("cached DCR response did not authorize real SDK connection: %+v", status)
	}
	if _, err := manager.Authorize(ctx, "oauth", redirect+"-new"); err != nil || f.registrations.Load() != 2 {
		t.Fatalf("different callback reused previous registration: %v, calls=%d", err, f.registrations.Load())
	}
}

func TestOAuthRejectedClientIsRegisteredAgainOnNextExplicitAttempt(t *testing.T) {
	f := oauthServiceFixture(t)
	f.dynamicConfidential.Store(true)
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
	defer manager.Close()
	if _, err := manager.Apply(t.Context(), f.settings("")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	redirect := "http://127.0.0.1:9090" + mcpconfig.OAuthCallbackPath + "alice"
	view, err := manager.Authorize(ctx, "oauth", redirect)
	if err != nil {
		t.Fatal(err)
	}
	f.denyClient.Store(true)
	if _, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view)); err == nil {
		t.Fatal("invalid_client exchange unexpectedly authorized")
	} else if strings.Contains(err.Error(), "private-dynamic-client-secret") {
		t.Fatal("learned registration secret leaked through callback error")
	}
	f.denyClient.Store(false)
	view, err = manager.Authorize(ctx, "oauth", redirect)
	if err != nil || f.registrations.Load() != 2 {
		t.Fatalf("rejected client registration was reused: %v, calls=%d", err, f.registrations.Load())
	}
	if _, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view)); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthRepeatedStartRetainsTheActiveBrowserFlow(t *testing.T) {
	f := oauthServiceFixture(t)
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
	defer manager.Close()
	if _, err := manager.Apply(t.Context(), f.settings("")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	redirect := "http://127.0.0.1:9090" + mcpconfig.OAuthCallbackPath + "alice"
	first, err := manager.Authorize(ctx, "oauth", redirect)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Authorize(ctx, "oauth", redirect+"-different-origin-request")
	if err != nil || second.ID != first.ID || second.URL != first.URL || f.registrations.Load() != 1 {
		t.Fatalf("repeated start withdrew the active browser flow: %v", err)
	}
	if result, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, first)); err != nil || result.State != "authorized" {
		t.Fatalf("initial consent was invalidated by another start: %+v %v", result, err)
	}
}

func TestOAuthRegistrationPersistenceAndInvalidationBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	store, _ := newOAuthGrantStore(path)
	registration := oauthClientRegistration{Issuer: "https://issuer.example", Response: &oauthex.ClientRegistrationResponse{
		ClientID: "registered", ClientSecret: "private-registration-secret", ClientIDIssuedAt: time.Unix(1700000000, 0), ClientSecretExpiresAt: time.Now().Add(time.Hour).Truncate(time.Second),
		ClientRegistrationMetadata: oauthex.ClientRegistrationMetadata{TokenEndpointAuthMethod: "client_secret_post"},
	}}
	if err := store.saveClient(t.Context(), "key", registration); err != nil {
		t.Fatal(err)
	}
	reopened, err := newOAuthGrantStore(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reopened.credentials.ReadRecord(oauthClientRecordKey("key"))
	if err != nil || stored == nil {
		t.Fatal("client registration missing from shared credential service", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("registration secret file is not private")
	}
	loaded, found, err := reopened.client("key")
	if err != nil || !found || loaded.Response.ClientSecret != registration.Response.ClientSecret || loaded.Response.TokenEndpointAuthMethod != "client_secret_post" || !loaded.Response.ClientIDIssuedAt.Equal(registration.Response.ClientIDIssuedAt) {
		t.Fatal("cached registration lost secret, auth method or timestamp", err)
	}
	previous := loaded
	replacement := registration
	response := *registration.Response
	response.ClientID = "replacement"
	replacement.Response = &response
	if err := store.saveClient(t.Context(), "key", replacement); err != nil {
		t.Fatal(err)
	}
	if err := store.invalidateClient(t.Context(), "key", previous); err != nil {
		t.Fatal(err)
	}
	if current, found, err := store.client("key"); err != nil || !found || current.Response.ClientID != "replacement" {
		t.Fatal("stale rejection deleted replacement registration", err)
	}
	response.ClientSecretExpiresAt = time.Now().Add(-time.Hour)
	if err := store.saveClient(t.Context(), "key", replacement); err != nil {
		t.Fatal(err)
	}
	if current, found, err := store.client("key"); err != nil || !found || current.Response.ClientSecretExpiresAt.Unix() != response.ClientSecretExpiresAt.Unix() {
		t.Fatal("client provider changed the upstream opaque registration read contract", err)
	}
}
