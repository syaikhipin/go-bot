package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-bot/internal/credentials"
	mcpconfig "go-bot/mcp"
)

// This optional differential test uses the actual pinned upstream source and
// SDK to refresh Go-written records against a real local OAuth server. Ordinary
// Go tests need neither npm installation nor network access to a model service.
func TestOAuthRecordsInteroperateWithPinnedUpstream(t *testing.T) {
	module := os.Getenv("GO_BOT_UPSTREAM_MCP_OAUTH_MODULE")
	if module == "" {
		t.Skip("set GO_BOT_UPSTREAM_MCP_OAUTH_MODULE to the compiled pinned upstream oauth-credentials module")
	}
	for _, scenario := range []struct {
		name, clientID string
		confidential   bool
	}{
		{"dynamic-public", "", false}, {"dynamic-confidential", "", true}, {"configured-public", "test-client", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := oauthServiceFixture(t)
			f.dynamicConfidential.Store(scenario.confidential)
			manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
			defer manager.Close()
			settings := f.settings(scenario.clientID)
			const key = "mcp-client/oracle"
			settings.Servers["oauth"].OAuth.CredentialKey = key
			if _, err := manager.Apply(t.Context(), settings); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view)); err != nil {
				t.Fatal(err)
			}
			rows, err := manager.oauthStore.credentials.ListRecords()
			if err != nil {
				t.Fatal(err)
			}
			records := map[string]*credentials.Record{}
			for _, row := range rows {
				records[row.Key], err = manager.oauthStore.credentials.ReadRecord(row.Key)
				if err != nil {
					t.Fatal(err)
				}
			}
			grant, err := manager.oauthStore.connectionGrant(key, manager.oauthConfigBinding("oauth", "", settings.Servers["oauth"]))
			if err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(map[string]any{"records": records, "key": key, "url": settings.Servers["oauth"].URL, "clientId": scenario.clientID, "accessToken": grant.Token.AccessToken})
			script, err := filepath.Abs("../../tests/upstream-plugins/mcp-oauth-oracle.mjs")
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(ctx, "node", script, module)
			command.Stdin = bytes.NewReader(input)
			var stderr bytes.Buffer
			command.Stderr = &stderr
			output, err := command.Output()
			if err != nil {
				t.Fatalf("pinned upstream record/refresh oracle: %v %s", err, stderr.String())
			}
			var result struct {
				Records   map[string]*credentials.Record `json:"records"`
				Refreshed bool                           `json:"refreshed"`
			}
			if err := json.Unmarshal(output, &result); err != nil || !result.Refreshed || f.refreshes.Load() != 1 {
				t.Fatal("upstream oracle did not perform one actual refresh", err)
			}
			for name, record := range result.Records {
				if _, err := manager.oauthStore.credentials.ModifyRecord(ctx, name, func(*credentials.Record) (*credentials.Record, error) { return record, nil }); err != nil {
					t.Fatal(err)
				}
			}
			status, err := manager.Restart(ctx, "oauth")
			if err != nil || len(status.Servers) != 1 || status.Servers[0].State != "connected" {
				t.Fatalf("Go could not reconnect using the upstream-refreshed grant: %v", err)
			}
		})
	}
}

func TestOAuthGrantUsesUpstreamPayloadWithoutGoConfigOrDuplicatedClientSecret(t *testing.T) {
	grant := fixtureOAuthGrant()
	grant.ScopePresent = true // Explicit empty scope remains different from omission.
	record, err := oauthRecord("mcp-oauth-grant", grant)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(record.Payload, &value); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"version", "type", "id", "serverUrl", "issuer", "clientKey", "clientId", "metadata", "tokens", "expiresAt"} {
		if _, ok := value[field]; !ok {
			t.Fatalf("missing upstream field %s", field)
		}
	}
	for _, field := range []string{"binding", "config", "token", "scope"} {
		if _, ok := value[field]; ok {
			t.Fatalf("private Go representation leaked through %s", field)
		}
	}
	if strings.Contains(string(record.Payload), grant.Config.ClientSecret) {
		t.Fatal("client secret duplicated in grant")
	}
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(value["tokens"], &tokens); err != nil {
		t.Fatal(err)
	}
	if string(tokens["scope"]) != `""` {
		t.Fatal("explicit empty token scope was lost")
	}
	var decoded oauthGrant
	if err := decodeOAuthRecord(record, "mcp-oauth-grant", &decoded); err != nil || decoded.Binding != grant.Binding || !decoded.ScopePresent || !sameOAuthToken(decoded.Token, grant.Token) {
		t.Fatal("upstream record did not restore the bound token", err)
	}
}

func TestOAuthRefreshRejectsMissingOrReplacedClientInformation(t *testing.T) {
	for _, scenario := range []string{"deleted", "different-issuer", "different-client", "wrong-kind"} {
		t.Run(scenario, func(t *testing.T) {
			requests := 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; http.Error(w, "must not refresh", 400) }))
			defer remote.Close()
			store, err := newOAuthGrantStore(filepath.Join(t.TempDir(), credentials.Filename))
			if err != nil {
				t.Fatal(err)
			}
			defer store.credentials.Close()
			grant := fixtureOAuthGrant()
			grant.Config.Endpoint.TokenURL = remote.URL
			saveFixtureOAuthClient(t, store, &grant)
			if err := store.put(t.Context(), "mail", grant); err != nil {
				t.Fatal(err)
			}
			if scenario == "deleted" {
				if err := store.credentials.DeleteRecord(t.Context(), grant.ClientKey); err != nil {
					t.Fatal(err)
				}
			} else {
				_, err = store.credentials.ModifyRecord(t.Context(), grant.ClientKey, func(record *credentials.Record) (*credentials.Record, error) {
					if scenario == "wrong-kind" {
						return &credentials.Record{Kind: "api-key"}, nil
					}
					var registration oauthClientRegistration
					if err := decodeOAuthRecord(record, "mcp-oauth-client", &registration); err != nil {
						return nil, err
					}
					if scenario == "different-issuer" {
						registration.Issuer = "https://other.example"
					} else {
						registration.Response.ClientID = "other-client"
					}
					return oauthRecord("mcp-oauth-client", registration)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err = store.refresh(t.Context(), "mail", grant.Binding, grant.ID, grant.Token.AccessToken, true, remote.Client())
			var detail *Error
			if !errors.As(err, &detail) || detail.AuthorizationReason != "binding" || requests != 0 {
				t.Fatalf("refresh did not enforce client binding: requests=%d err=%v", requests, err)
			}
			loaded, found, err := store.get("mail", grant.Binding)
			if err != nil || !found || loaded.Token.AccessToken != grant.Token.AccessToken {
				t.Fatal("rejected refresh modified the grant", err)
			}
		})
	}
}

func TestOAuthCanReadUpstreamRecordsFromTheSharedProvider(t *testing.T) {
	store, err := newOAuthGrantStore(filepath.Join(t.TempDir(), credentials.Filename))
	if err != nil {
		t.Fatal(err)
	}
	defer store.credentials.Close()
	// Upstream oauthRecord(): optional metadata and expiry need not be present.
	payload := json.RawMessage(`{"version":1,"type":"mcp-oauth-grant","id":"e477b6dc-20c1-431d-9fca-4cb56932bf13","serverUrl":"https://resource.example/mcp","issuer":"https://issuer.example","clientKey":"mcp-client/shared-client","clientId":"shared-client","configuredScope":"","tokens":{"access_token":"foreign-access","token_type":"Bearer","scope":"read"}}`)
	_, err = store.credentials.ModifyRecord(t.Context(), "mcp-client/shared", func(*credentials.Record) (*credentials.Record, error) {
		return &credentials.Record{Kind: "grant", Payload: payload}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	grant, err := store.connectionGrant("mcp-client/shared", oauthIdentityBinding("https://resource.example/mcp", nil, &empty))
	if err != nil || grant.Scope != "read" || !grant.Token.Expiry.IsZero() || grant.Config.ClientSecret != "" {
		t.Fatal("upstream grant could not be read", err)
	}
	if _, err := store.connectionGrant("mcp-client/shared", oauthIdentityBinding("https://resource.example/mcp", nil, nil)); err == nil {
		t.Fatal("configured empty scope crossed omitted scope binding")
	}
}
