package mcpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	mcpconfig "go-bot/mcp"
)

type oauthDiscoveryBarrier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *oauthDiscoveryBarrier) RoundTrip(r *http.Request) (*http.Response, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return http.DefaultTransport.RoundTrip(r)
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
}

func TestOAuthPluginStartRequiresVerifiedChannelAndAdmitsBeforeDiscovery(t *testing.T) {
	f := oauthServiceFixture(t)
	cwd := t.TempDir()
	barrier := &oauthDiscoveryBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	manager := NewManager(t.Context(), ManagerOptions{CWD: cwd, Connection: ConnectionOptions{HTTPClient: &http.Client{Transport: barrier}}})
	defer manager.Close()
	settings := f.settings("test-client")
	if _, err := manager.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartAuthorization(t.Context(), "oauth"); err == nil {
		t.Fatal("plugin guessed a callback without a Controller channel")
	} else {
		var detail *Error
		if !errors.As(err, &detail) || detail.Code != "MCP_OAUTH_CALLBACK_UNAVAILABLE" {
			t.Fatal("wrong absent-channel failure", err)
		}
	}
	observe := func(redirect string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "http://private-host/v1/host/stream", nil)
		r.Header.Set(mcpconfig.OAuthCallbackHeader, redirect)
		out := httptest.NewRecorder()
		manager.ControllerOAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(out, r)
		return out.Code
	}
	if observe("http://public-controller.example"+mcpconfig.OAuthCallbackPath+"alice") != http.StatusBadRequest {
		t.Fatal("non-loopback HTTP enabled OAuth")
	}
	redirect := "http://127.0.0.1:9090" + mcpconfig.OAuthCallbackPath + "alice"
	if observe(redirect) != http.StatusNoContent {
		t.Fatal("verified channel rejected")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	view, err := manager.StartAuthorization(ctx, "oauth")
	if err != nil || view.ID == "" || view.Phase != "starting" {
		t.Fatalf("start did not admit independently of blocked discovery: %+v %v", view, err)
	}
	select {
	case <-barrier.entered:
	case <-ctx.Done():
		t.Fatal("actual discovery did not start", ctx.Err())
	}
	secondRedirect := "http://127.0.0.1:9091" + mcpconfig.OAuthCallbackPath + "alice"
	if observe(secondRedirect) != http.StatusNoContent {
		t.Fatal("new Controller route rejected")
	}
	if second, err := manager.StartAuthorization(ctx, "oauth"); err != nil || second.ID != view.ID {
		t.Fatal("route refresh replaced in-flight authorization", err)
	}
	close(barrier.release)
	var authorization mcpconfig.OAuthAuthorization
	for {
		changed, _ := manager.Changes()
		current := manager.Status().Servers[0].Authorization
		if current != nil && current.Phase == "waiting-browser" {
			authorization = *current
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("discovery did not publish URL", ctx.Err())
		}
	}
	parsed, _ := url.Parse(authorization.URL)
	if parsed.Query().Get("redirect_uri") != redirect {
		t.Fatal("a route refresh changed a running flow's bound redirect")
	}
	if result, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, authorization)); err != nil || result.State != "authorized" {
		t.Fatalf("async start could not finish actual exchange: %+v %v", result, err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := NewManager(t.Context(), ManagerOptions{CWD: cwd})
	defer reopened.Close()
	if _, err := reopened.Apply(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.StartAuthorization(ctx, "oauth"); err == nil {
		t.Fatal("live Controller callback was incorrectly persisted as deployment configuration")
	}
}
