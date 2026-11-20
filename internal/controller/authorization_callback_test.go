package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go-bot/internal/config"
	"go-bot/internal/httpapi"
	"go-bot/plugin"
)

func TestPluginCallbackForwardsOnlyToTheAccountsAuthenticatedHost(t *testing.T) {
	token := uuid.NewString()
	host := httptest.NewServer(httpapi.Authenticate("host-private", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != plugin.InternalPath+"/authorization/callback" {
			http.NotFound(w, r)
			return
		}
		var input struct {
			Token string
			Query map[string][]string
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Token != token || input.Query["state"][0] != "provider-state" || input.Query["code"][0] != "private-code" {
			t.Errorf("incorrect callback routing: %+v", input)
		}
		writeJSON(w, 200, map[string]bool{"received": true})
	})))
	defer host.Close()
	store := &memoryStore{records: map[string]Account{"alice": {DesiredState: running, HostEndpoint: host.URL, HostToken: "host-private"}}}
	manager := New(config.Default(), nil, store)
	r := httptest.NewRequest("GET", plugin.AuthorizationCallbackPath+"alice/"+token+"?state=provider-state&code=private-code", nil)
	r.Header.Set("Origin", "https://provider.example")
	w := httptest.NewRecorder()
	manager.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Authorization received") || strings.Contains(w.Body.String(), "private-code") || strings.Contains(w.Body.String(), "host-private") {
		t.Fatalf("callback page: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"alice/not-a-token?state=x", "alice/" + token + "?state=x&state=y", "unknown/" + token + "?state=x"} {
		w := httptest.NewRecorder()
		manager.Handler().ServeHTTP(w, httptest.NewRequest("GET", plugin.AuthorizationCallbackPath+path, nil))
		if w.Code < 400 {
			t.Fatalf("accepted invalid callback: %s", path)
		}
	}
	if actual := pluginCallbackBase("https://controller.example/v1/mcp/oauth/callback/alice"); actual != "https://controller.example"+plugin.AuthorizationCallbackPath+"alice/" {
		t.Fatal(actual)
	}
}
