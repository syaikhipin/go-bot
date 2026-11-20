package authorization

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"go-bot/internal/credentials"
)

func TestCallbackIsBoundToAttemptAndDoesNotAuthorizeWithoutCommit(t *testing.T) {
	service, store := fixture(t)
	surface := NewSurface(t.Context(), service, nil)
	t.Cleanup(surface.Close)
	prepared := make(chan string, 1)
	received := make(chan map[string][]string, 1)
	commit := make(chan struct{})
	register(t, service, "mail/account", func(session *Session) error {
		address, err := session.Callback(session.Context, "library-generated-state")
		if err != nil {
			return err
		}
		prepared <- address
		query, err := session.WaitCallback(session.Context)
		if err != nil {
			return err
		}
		received <- query
		select {
		case <-commit:
		case <-session.Context.Done():
			return session.Context.Err()
		}
		return session.Commit(&credentials.Record{Kind: "grant", Payload: json.RawMessage(`{"access_token":"test-token-after-validation"}`)})
	})
	ctx := WithCallbackBase(t.Context(), "https://controller.example/v1/plugins/authorization/callback/alice/")
	result, err := surface.Handle(ctx, "start", json.RawMessage(`{"key":"mail/account"}`))
	if err != nil {
		t.Fatal(err)
	}
	view := result.(View)
	var address string
	select {
	case address = <-prepared:
	case <-time.After(3 * time.Second):
		t.Fatal("callback not prepared")
	}
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	token := u.Path[strings.LastIndex(u.Path, "/")+1:]
	query := map[string][]string{"state": {"wrong"}, "code": {"test-code"}, "iss": {"https://provider.example"}}
	if _, err := surface.receiveCallback(token, query); err == nil {
		t.Fatal("accepted a foreign state")
	}
	query["state"][0] = "library-generated-state"
	if _, err := surface.receiveCallback(token, query); err != nil {
		t.Fatal(err)
	}
	if _, err := surface.receiveCallback(token, query); err == nil {
		t.Fatal("accepted callback replay")
	}
	select {
	case values := <-received:
		if values["code"][0] != "test-code" {
			t.Fatal(values)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("plugin did not receive callback")
	}
	if record, err := store.ReadRecord("mail/account"); err != nil || record != nil {
		t.Fatal("callback receipt fabricated a credential", err)
	}
	current, err := surface.Handle(t.Context(), "get", json.RawMessage(`{"id":"`+view.ID+`"}`))
	if err != nil || current.(View).Status != "running" {
		t.Fatal("callback receipt fabricated success", current, err)
	}
	close(commit)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, _ = surface.Handle(t.Context(), "get", json.RawMessage(`{"id":"`+view.ID+`"}`))
		if current.(View).Status == "authorized" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if current.(View).Status != "authorized" {
		t.Fatalf("callback flow did not commit: %+v", current)
	}
	data, _ := json.Marshal(current)
	if strings.Contains(string(data), "test-code") || strings.Contains(string(data), "test-token-after-validation") {
		t.Fatal("private callback data entered the UI view")
	}
}

func TestCallbackCancellationWithdrawsPendingReceiver(t *testing.T) {
	service, store := fixture(t)
	surface := NewSurface(t.Context(), service, nil)
	t.Cleanup(surface.Close)
	prepared := make(chan string, 1)
	returned := make(chan error, 1)
	register(t, service, "mail/account", func(session *Session) error {
		address, err := session.Callback(session.Context, "expected-state")
		if err != nil {
			return err
		}
		prepared <- address
		_, err = session.WaitCallback(session.Context)
		returned <- err
		return err
	})
	ctx := WithCallbackBase(t.Context(), "http://localhost:8080/v1/plugins/authorization/callback/alice/")
	value, err := surface.Handle(ctx, "start", json.RawMessage(`{"key":"mail/account"}`))
	if err != nil {
		t.Fatal(err)
	}
	var address string
	select {
	case address = <-prepared:
	case <-time.After(3 * time.Second):
		t.Fatal("no callback")
	}
	attempt := value.(View)
	value, err = surface.Handle(t.Context(), "cancel", json.RawMessage(`{"id":"`+attempt.ID+`"}`))
	if err != nil || value.(View).Status != "cancelled" {
		t.Fatalf("cancel: %+v %v", value, err)
	}
	select {
	case err = <-returned:
		if err == nil {
			t.Fatal("wait reported success after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("callback wait leaked")
	}
	token := address[strings.LastIndex(address, "/")+1:]
	if _, err := surface.receiveCallback(token, map[string][]string{"state": {"expected-state"}, "code": {"late-code"}}); err == nil {
		t.Fatal("accepted callback after cancellation")
	}
	if record, err := store.ReadRecord("mail/account"); err != nil || record != nil {
		t.Fatal("cancel saved a credential", err)
	}
}

func TestCallbackRejectsUnverifiedOrigins(t *testing.T) {
	for _, base := range []string{"", "http://controller.example/callback/", "https://user:password@controller.example/callback/", "https://controller.example/callback/?injected=1", "https://controller.example/callback/#fragment", "https://controller.example/callback"} {
		t.Run(base, func(t *testing.T) {
			surface := &Surface{}
			attempt := &surfaceAttempt{ctx: t.Context(), view: View{Status: "running"}, callbackBase: base}
			if _, err := surface.prepareCallback(t.Context(), attempt, "state"); err == nil {
				t.Fatal("accepted unverified callback base")
			}
		})
	}
}
