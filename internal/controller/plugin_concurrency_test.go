package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go-bot/extensions/catalog"
	"go-bot/internal/config"
	"go-bot/plugin"
)

func TestSlowPluginInstallDoesNotBlockAccountRequests(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == plugin.InternalPath+"/packages/inspect":
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			writeJSON(w, 200, plugin.PackageInfo{Name: "fixture", Version: "1.0.0", API: "1"})
		case r.URL.Path == "/readyz":
			w.WriteHeader(200)
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSON(w, 200, plugin.Status{Runtime: "running"})
		}
	}))
	defer host.Close()
	// Unblock the fake Host before its server cleanup if an assertion fails.
	defer unblock()
	store := &memoryStore{records: map[string]Account{"alice": {DesiredState: running, HostEndpoint: host.URL, HostToken: "fixture"}}}
	manager := New(config.Default(), nil, store)
	manager.pluginCatalog = []catalog.Entry{{CatalogEntry: plugin.CatalogEntry{ID: "fixture", Package: "fixture", Version: "1.0.0"}, Source: plugin.Source{Type: "registry", Spec: "fixture@1.0.0"}}}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set(UserHeader, "alice")
		out := httptest.NewRecorder()
		manager.Handler().ServeHTTP(out, r)
		return out
	}
	installed := make(chan *httptest.ResponseRecorder, 1)
	go func() { installed <- request("POST", plugin.SettingsPath+"/packages", `{"pluginId":"fixture"}`) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("install did not reach the Host")
	}
	for _, path := range []string{plugin.SettingsPath, settingsPath} {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- request("GET", path, "") }()
		select {
		case response := <-done:
			if response.Code != 200 {
				t.Fatalf("%s: %d %s", path, response.Code, response.Body)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s blocked behind package installation", path)
		}
	}
	opened := make(chan error, 1)
	go func() { opened <- manager.Ensure(context.Background(), "alice") }()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("opening the account waited for plugin installation")
	}
	unblock()
	select {
	case response := <-installed:
		if response.Code != 200 {
			t.Fatalf("install: %d %s", response.Code, response.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("install did not finish")
	}
}
