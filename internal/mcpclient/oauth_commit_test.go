package mcpclient

import (
	"context"
	"go-bot/internal/credentials"
	"path/filepath"
	"testing"
	"time"

	mcpconfig "go-bot/mcp"
)

func TestOAuthAdmittedCommitDrainsBeforeCancelSignOutOrShutdown(t *testing.T) {
	for _, operation := range []string{"cancel", "sign-out", "shutdown"} {
		t.Run(operation, func(t *testing.T) {
			f := oauthServiceFixture(t)
			cwd := t.TempDir()
			manager := NewManager(t.Context(), ManagerOptions{CWD: cwd})
			defer manager.Close()
			settings := f.settings("test-client")
			if _, err := manager.Apply(t.Context(), settings); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
			if err != nil {
				t.Fatal(err)
			}
			callback := visitOAuthAuthorization(t, view)
			// Hold the shared Go credential provider immediately before publication. This
			// observes actual SDK exchange/commit admission, without timing sleeps
			// or replacing the credential writer with a successful test double.
			barrier, releaseBarrier := make(chan struct{}), make(chan struct{})
			barrierDone := make(chan error, 1)
			go func() {
				_, err := manager.oauthStore.credentials.ModifyRecord(t.Context(), "mcp/commit-barrier", func(*credentials.Record) (*credentials.Record, error) {
					close(barrier)
					<-releaseBarrier
					return &credentials.Record{Kind: "api-key"}, nil
				})
				barrierDone <- err
			}()
			<-barrier
			locked := true
			defer func() {
				if locked {
					close(releaseBarrier)
					<-barrierDone
				}
			}()
			type completion struct {
				view mcpconfig.OAuthAuthorization
				err  error
			}
			completed := make(chan completion, 1)
			go func() {
				result, err := manager.CompleteAuthorization(ctx, callback)
				completed <- completion{result, err}
			}()
			var attempt *oauthAttempt
			for {
				changed, _ := manager.Changes()
				manager.mu.RLock()
				attempt = manager.authorizations["oauth"]
				admitted := attempt != nil && attempt.view.Phase == "committing"
				manager.mu.RUnlock()
				if admitted {
					break
				}
				select {
				case <-changed:
				case <-ctx.Done():
					t.Fatal("real token exchange did not reach commit admission", ctx.Err())
				}
			}
			stopped := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "cancel":
					err = manager.CancelAuthorizationAttempt(ctx, "oauth", view.ID)
				case "sign-out":
					_, err = manager.DisconnectAuthorization(ctx, "oauth")
				case "shutdown":
					err = manager.Close()
				}
				stopped <- err
			}()
			select {
			case <-attempt.ctx.Done():
			case <-ctx.Done():
				t.Fatal("owner did not withdraw authorization", ctx.Err())
			}
			select {
			case err := <-stopped:
				t.Fatalf("owner completed before admitted write drained: %v", err)
			default:
			}
			close(releaseBarrier)
			if err := <-barrierDone; err != nil {
				t.Fatal(err)
			}
			locked = false
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal("owner did not finish admitted commit and cleanup", err)
				}
			case <-ctx.Done():
				t.Fatal("cancel/commit/remount deadlock", ctx.Err())
			}
			select {
			case result := <-completed:
				if result.err != nil || result.view.State != "authorized" {
					t.Fatalf("admitted commit was reported as withdrawn: %+v, %v", result.view, result.err)
				}
			case <-ctx.Done():
				t.Fatal("callback did not settle", ctx.Err())
			}
			store := manager.oauthStore
			if operation == "shutdown" {
				var err error
				store, err = newOAuthGrantStore(filepath.Join(cwd, ".dsh", ".credentials.yaml"))
				if err != nil {
					t.Fatal(err)
				}
			}
			grant, found, err := store.get("oauth", manager.oauthConfigBinding("oauth", "", settings.Servers["oauth"]))
			if err != nil || found != (operation != "sign-out") || (found && grant.Token.AccessToken == "") {
				t.Fatalf("invalid post-commit credential lifecycle: found=%v, err=%v", found, err)
			}
			if operation == "cancel" && manager.Status().Servers[0].State != "connected" {
				t.Fatal("cancelled admitted commit was not activated synchronously")
			}
			if operation == "sign-out" && manager.Status().Servers[0].State != "waiting-authorization" {
				t.Fatal("sign-out did not remount without the committed grant")
			}
		})
	}
}

func TestOAuthCommittedGrantIsIndependentOfManagedConnectionFailure(t *testing.T) {
	f := oauthServiceFixture(t)
	manager := NewManager(t.Context(), ManagerOptions{CWD: t.TempDir()})
	defer manager.Close()
	settings := f.settings("test-client")
	if _, err := manager.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	view, err := manager.Authorize(ctx, "oauth", "http://127.0.0.1:9090"+mcpconfig.OAuthCallbackPath+"alice")
	if err != nil {
		t.Fatal(err)
	}
	f.denyResource.Store(true)
	result, err := manager.CompleteAuthorization(ctx, visitOAuthAuthorization(t, view))
	if err != nil || result.State != "authorized" {
		t.Fatalf("MCP outage retracted successful credential commit: %+v, %v", result, err)
	}
	if _, found, err := manager.oauthStore.get("oauth", manager.oauthConfigBinding("oauth", "", settings.Servers["oauth"])); err != nil || !found {
		t.Fatal("MCP outage lost successful grant", err)
	}
	status := manager.Status().Servers[0]
	if status.State == "connected" || status.Authorization == nil || status.Authorization.State != "authorized" {
		t.Fatalf("credential and connection outcomes were conflated: %+v", status)
	}
}
