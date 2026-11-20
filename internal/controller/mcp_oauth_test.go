package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go-bot/internal/config"
	"go-bot/internal/hostruntime"
	"go-bot/internal/httpapi"
	"go-bot/internal/mcpclient"
	mcpconfig "go-bot/mcp"
)

func TestMCPOAuthRedirectUsesActualHostWithoutTrustingForwardedDestination(t *testing.T) {
	for _, test := range []struct {
		target, origin, forwardedHost, forwardedProto, want string
		valid                                               bool
	}{
		{"http://127.0.0.1:8080/start", "", "attacker.example", "https", "http://127.0.0.1:8080" + mcpconfig.OAuthCallbackPath + "alice", true},
		{"http://controller.example/start", "https://controller.example", "attacker.example", "http", "https://controller.example" + mcpconfig.OAuthCallbackPath + "alice", true},
		{"http://controller.example/start", "", "controller.example", "https", "", false},
		{"https://controller.example/start", "https://attacker.example", "", "", "", false},
		{"https://controller.example/start", "https://controller.example/elsewhere", "", "", "", false},
		{"https://controller.example/start", "https://user@controller.example", "", "", "", false},
	} {
		r := httptest.NewRequest(http.MethodPost, test.target, nil)
		r.Header.Set("X-Forwarded-Host", test.forwardedHost)
		r.Header.Set("X-Forwarded-Proto", test.forwardedProto)
		redirect, err := mcpOAuthRedirect(r, test.origin, "alice")
		if (err == nil) != test.valid || redirect != test.want {
			t.Fatalf("%+v -> %q %v", test, redirect, err)
		}
	}
}

func TestControllerRejectsAmbiguousOAuthParametersBeforeHostAdmission(t *testing.T) {
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"received"}`))
	}))
	defer host.Close()
	store := &memoryStore{records: map[string]Account{"alice": {DesiredState: running, HostID: "callback-run", HostEndpoint: host.URL, HostToken: "private-fixture-token"}}}
	manager := New(config.Default(), nil, store)
	for _, query := range []string{
		"state=s&code=c&error=", "state=s&code=&error=access_denied", "state=s&code=c&error=server_error", "state=s&code=", "state=s&error=", "state=s", "code=c", "state=s&state=s&code=c", "state=s&code=c&code=c", "state=s&error=access_denied&error=access_denied", "state=s&code=c&iss=a&iss=b",
	} {
		response := httptest.NewRecorder()
		manager.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, mcpconfig.OAuthCallbackPath+"alice?"+query, nil))
		if response.Code != 400 {
			t.Fatalf("ambiguous callback admitted: %q status=%d", query, response.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid query reached Host")
	}
	response := httptest.NewRecorder()
	manager.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, mcpconfig.OAuthCallbackPath+"alice?state=s&code=c", nil))
	if response.Code != 200 || calls.Load() != 1 || !strings.Contains(response.Body.String(), "Authorization received") {
		t.Fatal("valid callback could not reach Host")
	}
}

func TestControllerSuppliesVerifiedCallbackOnPrivateAndProxiedHostRequests(t *testing.T) {
	for _, path := range []string{mcpconfig.RuntimePath, "/v1/host/stream"} {
		for _, item := range []struct{ target, origin, want string }{
			{"https://controller.example", "", "https://controller.example"},
			{"http://controller.example", "https://controller.example", "https://controller.example"},
			{"http://127.0.0.1:8080", "", "http://127.0.0.1:8080"},
			{"http://controller.example", "", ""},
			{"http://controller.example", "https://attacker.example", ""},
		} {
			t.Run(path+"/"+item.target+"/"+item.origin, func(t *testing.T) {
				observed := make(chan string, 1)
				host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					observed <- r.Header.Get(mcpconfig.OAuthCallbackHeader)
					if r.Header.Get("Authorization") != "Bearer private-callback-token" || r.Header.Get(UserHeader) != "" || r.Header.Get(mcpconfig.ControllerOriginHeader) != "" {
						t.Error("private routing identity was copied from external headers")
					}
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(`{"servers":[]}`))
				}))
				defer host.Close()
				store := &memoryStore{records: map[string]Account{"alice": {DesiredState: running, HostID: "callback-run", HostEndpoint: host.URL, HostToken: "private-callback-token"}}}
				manager := New(config.Default(), nil, store)
				r := httptest.NewRequest(http.MethodGet, item.target+path, nil)
				r.Header.Set(UserHeader, "alice")
				r.Header.Set(mcpconfig.ControllerOriginHeader, item.origin)
				r.Header.Set(mcpconfig.OAuthCallbackHeader, "https://attacker.example"+mcpconfig.OAuthCallbackPath+"other")
				r.Header.Set("X-Forwarded-Host", "attacker.example")
				r.Header.Set("X-Forwarded-Proto", "https")
				out := httptest.NewRecorder()
				manager.Handler().ServeHTTP(out, r)
				want := item.want
				if want != "" {
					want += mcpconfig.OAuthCallbackPath + "alice"
				}
				var callback string
				select {
				case callback = <-observed:
				default:
					t.Fatal("Controller did not reach the Host")
				}
				if out.Code != 200 || callback != want {
					t.Fatalf("callback routing: status=%d got=%q want=%q", out.Code, callback, want)
				}
			})
		}
	}
}

func TestOAuthProviderBrowserCallbackPassesControllerToRealHostAndKeepsOtherAPIsProtected(t *testing.T) {
	testControllerOAuthFlow(t)
}

func testControllerOAuthFlow(t *testing.T) {
	var service *httptest.Server
	var tokenRequests atomic.Int32
	var challengeMu sync.Mutex
	challenge := ""
	remote := sdk.NewServer(&sdk.Implementation{Name: "full-oauth-fixture", Version: "1"}, nil)
	remote.AddTool(&sdk.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	mcpHTTP := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return remote }, nil)
	service = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(value any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(value)
		}
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			write(map[string]any{"resource": service.URL + "/mcp", "authorization_servers": []string{service.URL}, "scopes_supported": []string{"read"}})
		case "/.well-known/oauth-authorization-server":
			write(map[string]any{"issuer": service.URL, "authorization_endpoint": service.URL + "/authorize", "token_endpoint": service.URL + "/token", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "authorization_response_iss_parameter_supported": true})
		case "/authorize":
			values := r.URL.Query()
			challengeMu.Lock()
			challenge = values.Get("code_challenge")
			challengeMu.Unlock()
			callback, _ := url.Parse(values.Get("redirect_uri"))
			query := callback.Query()
			query.Set("code", "one-time-test-code")
			query.Set("state", values.Get("state"))
			query.Set("iss", service.URL)
			callback.RawQuery = query.Encode()
			http.Redirect(w, r, callback.String(), http.StatusFound)
		case "/token":
			tokenRequests.Add(1)
			_ = r.ParseForm()
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			challengeMu.Lock()
			expected := challenge
			challengeMu.Unlock()
			if r.Form.Get("code") != "one-time-test-code" || expected == "" || base64.RawURLEncoding.EncodeToString(digest[:]) != expected {
				http.Error(w, "invalid PKCE exchange", 400)
				return
			}
			write(map[string]any{"access_token": "controller-fixture-private-token", "refresh_token": "controller-fixture-private-refresh", "expires_in": 3600, "token_type": "Bearer"})
		case "/mcp":
			if r.Header.Get("Authorization") != "Bearer controller-fixture-private-token" {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+service.URL+`/.well-known/oauth-protected-resource/mcp"`)
				http.Error(w, "authorization required", 401)
				return
			}
			mcpHTTP.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer service.Close()
	hostManager := mcpclient.NewManager(t.Context(), mcpclient.ManagerOptions{CWD: t.TempDir()})
	defer hostManager.Close()
	settings := mcpconfig.Settings{Servers: map[string]mcpconfig.Server{"oauth": {Transport: "streamable-http", URL: service.URL + "/mcp", OAuth: &mcpconfig.OAuth{ClientID: "fixture-client"}}}}
	if _, err := hostManager.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	native := &hostruntime.NativeHost{UserID: "alice", MCPChanges: hostManager.Changes}
	streamHandler := httpapi.NewNativeHostHTTP(native, httpapi.NativeWorkerHTTPOptions{})
	hostHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/host/stream" {
			streamHandler.ServeHTTP(w, r)
			return
		}
		hostManager.Handler().ServeHTTP(w, r)
	})
	host := httptest.NewServer(httpapi.Authenticate("private-oauth-host", hostManager.ControllerOAuth(hostHandler)))
	defer host.Close()
	store := &memoryStore{records: map[string]Account{
		"alice":   {DesiredState: running, HostID: "oauth-fixture-run", HostEndpoint: host.URL, HostToken: "private-oauth-host"},
		"stopped": {DesiredState: stopped},
	}}
	controller := New(config.Default(), nil, store)
	public := httptest.NewServer(controller.Handler())
	defer public.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var authorization mcpconfig.OAuthAuthorization
	var response *http.Response
	var err error

	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, public.URL+mcpconfig.RuntimePath+"/servers/oauth/authorize", strings.NewReader(`{"origin":"`+public.URL+`"}`))
	request.Header.Set(UserHeader, "alice")
	request.Header.Set("Content-Type", "application/json")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("start=%d %s", response.StatusCode, data)
	}
	err = json.NewDecoder(response.Body).Decode(&authorization)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	if authorization.ID == "" || authorization.Phase != "waiting-browser" {
		t.Fatalf("authorization identity missing: %+v", authorization)
	}
	staleCancel, _ := http.NewRequestWithContext(ctx, http.MethodDelete, public.URL+mcpconfig.RuntimePath+"/servers/oauth/authorization?id=older-attempt", nil)
	staleCancel.Header.Set(UserHeader, "alice")
	staleResponse, err := client.Do(staleCancel)
	if err != nil {
		t.Fatal(err)
	}
	staleResponse.Body.Close()
	if staleResponse.StatusCode != http.StatusOK {
		t.Fatalf("stale cancellation failed: %d", staleResponse.StatusCode)
	}
	response, err = client.Get(authorization.URL)
	if err != nil {
		t.Fatal(err)
	}
	callback, err := response.Location()
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	wrong := *callback
	wrong.Path = mcpconfig.OAuthCallbackPath + "stopped"
	response, err = client.Get(wrong.String())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 409 || tokenRequests.Load() != 0 {
		t.Fatal("callback started a stopped Host or exchanged another account's code")
	}
	callbackRequest, _ := http.NewRequestWithContext(ctx, http.MethodGet, callback.String(), nil)
	callbackRequest.Header.Set("Origin", service.URL)
	callbackRequest.Header.Set("Sec-Fetch-Site", "cross-site")
	response, err = client.Do(callbackRequest)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(page), "Authorization received") || strings.Contains(string(page), "one-time-test-code") || strings.Contains(string(page), "private-token") {
		t.Fatalf("callback=%d %s", response.StatusCode, page)
	}
	for {
		changed, _ := hostManager.Changes()
		servers := hostManager.Status().Servers
		if len(servers) == 1 && servers[0].State == "connected" && tokenRequests.Load() == 1 {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("Controller callback did not activate the real Host connection", ctx.Err())
		}
	}
	response, err = client.Get(callback.String())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 || tokenRequests.Load() != 1 {
		t.Fatal("callback replay reached token exchange")
	}
	ordinary, _ := http.NewRequest(http.MethodGet, public.URL+mcpconfig.RuntimePath, nil)
	ordinary.Header.Set(UserHeader, "alice")
	ordinary.Header.Set("Origin", service.URL)
	ordinary.Header.Set("Sec-Fetch-Site", "cross-site")
	response, err = client.Do(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("OAuth callback exemption relaxed ordinary API origin checks")
	}
}
