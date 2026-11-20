package mcpconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOriginalOAuthBudgetsAndScopeValidateAndSurviveCloning(t *testing.T) {
	for _, value := range []string{
		`{"scope":"","requestTimeoutMs":1,"authorizationTimeoutMs":2147483647}`,
		`{"scope":"read offline_access"}`,
		`{}`,
	} {
		var oauth OAuth
		if err := json.Unmarshal([]byte(value), &oauth); err != nil {
			t.Fatal(err)
		}
		s := Settings{Servers: map[string]Server{"mail": {Transport: "streamable-http", URL: "https://example.com/mcp", OAuth: &oauth}}}
		if err := s.Validate(); err != nil {
			t.Fatal("upstream options rejected", err)
		}
		cloned := s.Clone().Servers["mail"].OAuth
		if (cloned.Scope == nil) != (oauth.Scope == nil) || (cloned.Scope != nil && *cloned.Scope != *oauth.Scope) {
			t.Fatal("explicit empty scope did not survive cloning")
		}
	}
	for _, value := range []string{
		`{"requestTimeoutMs":0}`, `{"authorizationTimeoutMs":0}`,
		`{"requestTimeoutMs":-1}`, `{"authorizationTimeoutMs":2147483648}`,
	} {
		var oauth OAuth
		_ = json.Unmarshal([]byte(value), &oauth)
		s := Server{Transport: "streamable-http", URL: "https://example.com/mcp", OAuth: &oauth}
		if err := s.Validate(); err == nil {
			t.Fatal("invalid explicit upstream budget accepted", value)
		}
	}
}

func TestMCPConfigChecksTransportCredentialAndOAuthBoundaries(t *testing.T) {
	for _, server := range []Server{
		{Transport: "stdio", Command: "python3", Args: []string{"server.py"}, Env: map[string]string{"KEY": "${credential:service/key}"}},
		{Transport: "streamable-http", URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "Bearer ${credential:service/key}"}},
		{Transport: "streamable-http", URL: "http://127.0.0.1:8000/mcp", OAuth: &OAuth{ClientID: "client"}},
	} {
		if err := server.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, server := range []Server{
		{Transport: "stdio", Command: "python3", URL: "https://example.com"},
		{Transport: "stdio", Command: "python3", CWD: "relative"},
		{Transport: "stdio", Command: "python3", Env: map[string]string{"BAD=KEY": "value"}},
		{Transport: "streamable-http", URL: "https://secret@example.com/mcp"},
		{Transport: "streamable-http", URL: "https://example.com/mcp", Command: "python3"},
		{Transport: "streamable-http", URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "key", "authorization": "other"}},
		{Transport: "streamable-http", URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "Bearer key"}, OAuth: &OAuth{}},
		{Transport: "streamable-http", URL: "https://example.com/mcp", Headers: map[string]string{"X-Key": "${credential:unterminated"}},
	} {
		if err := server.Validate(); err == nil {
			t.Fatalf("invalid config accepted: %+v", server)
		}
	}
}

func TestMCPReferencesResolveOnlyExplicitFieldsAndPublicViewNeverReturnsValues(t *testing.T) {
	settings := Settings{Servers: map[string]Server{"knowledge": {Transport: "streamable-http", URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "Bearer ${credential:service/key}"}}}, Credentials: map[string]string{"service/key": "private-value", "unrelated": "do-not-forward"}}
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	resolved, missing, err := settings.Resolve("knowledge")
	if err != nil || len(missing) != 0 || resolved.Headers["Authorization"] != "Bearer private-value" {
		t.Fatalf("resolved=%+v missing=%v err=%v", resolved, missing, err)
	}
	data, _ := json.Marshal(resolved)
	if strings.Contains(string(data), "do-not-forward") {
		t.Fatal("unreferenced credential was forwarded")
	}
	public, _ := json.Marshal(settings.PublicView())
	if strings.Contains(string(public), "private-value") || strings.Contains(string(public), "do-not-forward") {
		t.Fatal("public settings exposed a private value")
	}
	delete(settings.Credentials, "service/key")
	_, missing, err = settings.Resolve("knowledge")
	if err != nil || len(missing) != 1 || missing[0] != "service/key" {
		t.Fatalf("missing=%v err=%v", missing, err)
	}
	settings.Credentials["service/key"] = "bad\nheader"
	if _, _, err = settings.Resolve("knowledge"); err == nil {
		t.Fatal("credential expansion permitted an invalid HTTP header")
	}
}

func TestPublicMCPViewMasksLiteralAuthenticationHeadersButPreservesReferences(t *testing.T) {
	settings := Settings{Servers: map[string]Server{"headers": {Transport: "streamable-http", URL: "https://example.com/mcp", Headers: map[string]string{
		"Authorization": "Bearer literal-private-key", "X-API-Key": "another-private-key", "X-Service-Token": "${credential:service/token}", "X-Tenant": "research",
	}}}}
	public := settings.PublicView()
	data, _ := json.Marshal(public)
	if strings.Contains(string(data), "literal-private-key") || strings.Contains(string(data), "another-private-key") {
		t.Fatal("literal authentication header leaked")
	}
	if public.Servers["headers"].Headers["X-Service-Token"] != "${credential:service/token}" || public.Servers["headers"].Headers["X-Tenant"] != "research" {
		t.Fatal("non-secret configuration was hidden")
	}
	if settings.Servers["headers"].Headers["Authorization"] != "Bearer literal-private-key" {
		t.Fatal("public projection modified private input")
	}
}

func TestResolvedOAuthClientSecretRemainsPrivateAcrossClones(t *testing.T) {
	settings := Settings{Servers: map[string]Server{"oauth": {Transport: "streamable-http", URL: "https://example.com/mcp", OAuth: &OAuth{ClientID: "client", ClientSecretRef: "oauth/client"}}}, Credentials: map[string]string{"oauth/client": "private-oauth-secret"}}
	server, missing, err := settings.Resolve("oauth")
	if err != nil || len(missing) != 0 || server.OAuth.ClientSecret != "private-oauth-secret" {
		t.Fatal("client secret reference did not resolve", err)
	}
	private := Settings{Servers: map[string]Server{"oauth": server}}
	if private.Clone().Servers["oauth"].OAuth.ClientSecret != "private-oauth-secret" {
		t.Fatal("Host clone lost private OAuth input")
	}
	data, _ := json.Marshal(private)
	if strings.Contains(string(data), "private-oauth-secret") {
		t.Fatal("private OAuth input entered serializable settings")
	}
	if private.PublicView().Servers["oauth"].OAuth.ClientSecret != "" {
		t.Fatal("public projection retained a private client secret")
	}
}
