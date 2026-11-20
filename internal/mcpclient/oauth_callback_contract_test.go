package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpconfig "go-bot/mcp"
)

type oauthTokenExchangeBarrier struct {
	endpoint         string
	entered, release chan struct{}
	once             sync.Once
}

func (b *oauthTokenExchangeBarrier) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.String() == b.endpoint && request.Method == http.MethodPost {
		b.once.Do(func() { close(b.entered) })
		select {
		case <-b.release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	}
	return http.DefaultTransport.RoundTrip(request)
}

func TestOAuthHTTPCallbackAcknowledgesBeforeExchangeAndDoesNotOwnTheFlow(t *testing.T) {
	f := oauthServiceFixture(t)
	barrier := &oauthTokenExchangeBarrier{endpoint: f.provider.URL + "/token", entered: make(chan struct{}), release: make(chan struct{})}
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir(), Connection: ConnectionOptions{HTTPClient: &http.Client{Transport: barrier}}})
	defer manager.Close()
	if _, err := manager.Apply(t.Context(), f.settings("fixture")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if err != nil {
		t.Fatal(err)
	}
	callback := visitOAuthAuthorization(t, view)
	data, _ := json.Marshal(callback)
	requestCtx, requestCancel := context.WithCancel(ctx)
	req := httptest.NewRequest(http.MethodPost, mcpconfig.InternalPath+"/oauth/callback", bytes.NewReader(data)).WithContext(requestCtx)
	result := httptest.NewRecorder()
	answered := make(chan struct{})
	go func() { manager.Handler().ServeHTTP(result, req); close(answered) }()
	select {
	case <-answered:
	case <-ctx.Done():
		t.Fatal("callback waited for token exchange", ctx.Err())
	}
	requestCancel() // Closing the browser page does not cancel the admitted flow.
	var receipt mcpconfig.OAuthCallbackReceipt
	if result.Code != http.StatusOK || json.Unmarshal(result.Body.Bytes(), &receipt) != nil || receipt.State != "received" {
		t.Fatal("callback did not acknowledge admission")
	}
	select {
	case <-barrier.entered:
	case <-ctx.Done():
		t.Fatal("actual token exchange did not start", ctx.Err())
	}
	if row, err := manager.oauthStore.credentials.ReadRecord(oauthGrantKey("oauth")); err != nil || row != nil {
		t.Fatal("receipt was confused with a credential commit", err)
	}
	if _, err := manager.ReceiveAuthorization(callback); err == nil {
		t.Fatal("pending callback could be replayed")
	}
	close(barrier.release)
	for {
		changed, _ := manager.Changes()
		servers := manager.Status().Servers
		if len(servers) == 1 && servers[0].State == "connected" && servers[0].Authorization != nil && servers[0].Authorization.Phase == "authorized" {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("browser disconnection cancelled the accepted authorization", ctx.Err())
		}
	}
}

func TestOAuthInvalidCallbacksLeaveTheSameBrowserFlowUsable(t *testing.T) {
	f := oauthServiceFixture(t)
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
	defer manager.Close()
	if _, err := manager.Apply(t.Context(), f.settings("test-client")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if err != nil {
		t.Fatal(err)
	}
	callback := visitOAuthAuthorization(t, view)
	for _, scenario := range []struct {
		name   string
		change func(*mcpconfig.OAuthCallback)
	}{
		{"wrong-state", func(v *mcpconfig.OAuthCallback) { v.State += "-wrong" }},
		{"wrong-issuer", func(v *mcpconfig.OAuthCallback) { v.Issuer += "/" }},
		{"missing-required-issuer", func(v *mcpconfig.OAuthCallback) { v.Issuer = "" }},
		{"neither-code-nor-error", func(v *mcpconfig.OAuthCallback) { v.Code = "" }},
		{"both-code-and-error", func(v *mcpconfig.OAuthCallback) { v.Error = "access_denied" }},
		{"unverified-denial", func(v *mcpconfig.OAuthCallback) {
			v.Code = ""
			v.Error = "access_denied"
			v.Issuer = "https://unrelated.example"
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			invalid := callback
			scenario.change(&invalid)
			if _, err := manager.CompleteAuthorization(ctx, invalid); err == nil {
				t.Fatal("invalid callback admitted")
			}
			manager.mu.RLock()
			attempt := manager.authorizations["oauth"]
			unchanged := attempt != nil && attempt.view.ID == view.ID && attempt.view.Phase == "waiting-browser" && !attempt.consumed && attempt.ctx.Err() == nil
			manager.mu.RUnlock()
			if !unchanged {
				t.Fatal("rejected callback consumed or cancelled the pending flow")
			}
			f.mu.Lock()
			exchanged := f.lastAccess != ""
			f.mu.Unlock()
			if exchanged {
				t.Fatal("rejected callback exchanged an authorization code")
			}
		})
	}
	if result, err := manager.CompleteAuthorization(ctx, callback); err != nil || result.Phase != "authorized" {
		t.Fatalf("valid callback after rejected inputs: phase=%s err=%v", result.Phase, err)
	}
	if _, err := manager.CompleteAuthorization(ctx, callback); err == nil {
		t.Fatal("accepted callback could be replayed")
	}
}

func TestOAuthCallbackIssuerUsesUpstreamDecisionTable(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		for _, returned := range []bool{false, true} {
			name := "advertised"
			if omitted {
				name = "not-advertised"
			}
			if returned {
				name += "/returned"
			} else {
				name += "/absent"
			}
			t.Run(name, func(t *testing.T) {
				f := oauthServiceFixture(t)
				f.omitIssuerAdvertisement.Store(omitted)
				manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
				defer manager.Close()
				if _, err := manager.Apply(t.Context(), f.settings("fixture")); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
				if err != nil {
					t.Fatal(err)
				}
				callback := visitOAuthAuthorization(t, view)
				if !returned {
					callback.Issuer = ""
				}
				result, err := manager.CompleteAuthorization(ctx, callback)
				if !omitted && !returned {
					if err == nil {
						t.Fatal("required iss was not enforced")
					}
					callback.Issuer = f.provider.URL
					result, err = manager.CompleteAuthorization(ctx, callback)
				}
				if err != nil || result.Phase != "authorized" {
					t.Fatalf("issuer table: phase=%s err=%v", result.Phase, err)
				}
			})
		}
	}
}

func TestOAuthCallbackSeparatesDeclineProviderFailureAndRejectedClient(t *testing.T) {
	for _, scenario := range []struct {
		code, phase   string
		registrations int32
	}{
		{"access_denied", "cancelled", 1},
		{"server_error", "failed", 1},
		{"invalid_client", "failed", 2},
		{"private-provider-error-must-not-be-displayed", "failed", 1},
	} {
		t.Run(scenario.code, func(t *testing.T) {
			f := oauthServiceFixture(t)
			manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
			defer manager.Close()
			if _, err := manager.Apply(t.Context(), f.settings("")); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			redirect := "http://127.0.0.1:9090" + mcpconfig.OAuthCallbackPath + "alice"
			view, err := manager.Authorize(ctx, "oauth", redirect)
			if err != nil {
				t.Fatal(err)
			}
			callback := visitOAuthAuthorization(t, view)
			callback.Code, callback.Error = "", scenario.code
			result, err := manager.CompleteAuthorization(ctx, callback)
			if result.Phase != scenario.phase {
				t.Fatalf("callback outcome: phase=%s err=%v", result.Phase, err)
			}
			if scenario.phase == "failed" && err == nil {
				t.Fatal("provider error was not reported")
			}
			if strings.Contains(result.Error, "private-provider-error") || (err != nil && strings.Contains(err.Error(), "private-provider-error")) {
				t.Fatal("untrusted provider error copied into diagnostics")
			}
			if grant, _ := manager.oauthStore.credentials.ReadRecord(oauthGrantKey("oauth")); grant != nil {
				t.Fatal("failed flow committed a grant")
			}
			view, err = manager.Authorize(ctx, "oauth", redirect)
			if err != nil || f.registrations.Load() != scenario.registrations {
				t.Fatalf("client registration lifecycle: calls=%d err=%v", f.registrations.Load(), err)
			}
			if result, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view)); err != nil || result.Phase != "authorized" {
				t.Fatalf("retry could not authorize: phase=%s err=%v", result.Phase, err)
			}
		})
	}
}
