package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go-bot/internal/config"
	"go-bot/internal/providersettings"
)

func TestImageProviderSettingsPersistAndSync(t *testing.T) {
	var lock sync.Mutex
	var applied providersettings.Selection
	reject := false
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		defer lock.Unlock()
		if r.URL.Path != "/internal/host/image-generation" || r.Header.Get("Authorization") != "Bearer host-token" {
			t.Error("wrong Host endpoint or credential")
		}
		if err := json.NewDecoder(r.Body).Decode(&applied); err != nil {
			t.Error(err)
		}
		if reject {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer host.Close()
	store := &memoryStore{records: map[string]Account{
		"alice": {DesiredState: running, HostEndpoint: host.URL, HostToken: "host-token"},
		"bob":   {},
	}}
	m := New(config.Default(), nil, store)
	call := func(id, method, path, body string, status int) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "127.0.0.1"
		r.Header.Set(UserHeader, id)
		w := httptest.NewRecorder()
		m.Handler().ServeHTTP(w, r)
		if w.Code != status || strings.Contains(w.Body.String(), "fixture-secret") {
			t.Fatalf("%s %s: HTTP %d", method, path, w.Code)
		}
		return w.Body.String()
	}
	call("alice", http.MethodPut, imageSettingsPath, `{"provider":"unknown","model":"ep-one"}`, 400)
	call("alice", http.MethodPut, imageSettingsPath, `{"provider":"seedream","model":"ep-one","apiKey":"fixture-secret"}`, 200)
	lock.Lock()
	initial := applied
	reject = true
	lock.Unlock()
	if initial.Model != "ep-one" || initial.APIKey != "fixture-secret" {
		t.Fatal("the running Host did not receive the selected model and key")
	}
	if got := call("alice", http.MethodPut, imageSettingsPath, `{"provider":"seedream","model":"ep-two"}`, 503); !strings.Contains(got, settingsSavedHostApplyFailed) {
		t.Fatalf("missing apply error: %s", got)
	}
	if got := call("alice", http.MethodGet, imageSettingsPath, "", 200); !strings.Contains(got, "ep-two") {
		t.Fatalf("saved model was lost after reload: %s", got)
	}
	lock.Lock()
	updated := applied
	reject = false
	lock.Unlock()
	saved, err := store.Get(t.Context(), "alice")
	if err != nil || saved.ImageGeneration.Entries["seedream"].Model != "ep-two" || updated.APIKey != "fixture-secret" {
		t.Fatal("model-only change did not preserve the key and persist")
	}
	if got := call("bob", http.MethodGet, imageSettingsPath, "", 200); strings.Contains(got, "ep-two") {
		t.Fatalf("another account's model was returned: %s", got)
	}
	call("alice", http.MethodPut, imageSettingsPath, `{"provider":"seedream","model":"ep-two"}`, 200)
	lock.Lock()
	if applied.Model != "ep-two" {
		lock.Unlock()
		t.Fatal("explicit retry did not apply saved settings")
	}
	lock.Unlock()
	removed := call("alice", http.MethodDelete, imageSettingsPath+"?provider=seedream", "", 200)
	lock.Lock()
	defer lock.Unlock()
	if !strings.Contains(removed, "ep-two") || applied.APIKey != "" {
		t.Fatal("removing the key did not clear the Host key and retain the model")
	}
}

func TestLegacySearchProviderRecord(t *testing.T) {
	store := &memoryStore{records: map[string]Account{
		"alice": {WebSearch: &providersettings.Settings{APIKey: "legacy-search-key"}},
	}}
	m := New(config.Default(), nil, store)
	r := httptest.NewRequest(http.MethodGet, webSearchSettingsPath, nil)
	r.Host = "127.0.0.1"
	r.Header.Set(UserHeader, "alice")
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"hasApiKey":true`) || strings.Contains(w.Body.String(), "legacy-search-key") {
		t.Fatalf("legacy settings response: HTTP %d", w.Code)
	}
}

func TestAdditionalProviderUsesTheSameSettingsContract(t *testing.T) {
	store := &memoryStore{records: map[string]Account{
		"alice": {WebSearch: &providersettings.Settings{APIKey: "legacy-search-key"}},
	}}
	m := New(config.Default(), nil, store)
	route := searchSettings
	route.catalog = func() []providersettings.Descriptor {
		return append(searchSettings.catalog(), providersettings.Descriptor{ID: "fixture", Name: "Fixture Search"})
	}
	call := func(method, body string) string {
		t.Helper()
		r := httptest.NewRequest(method, webSearchSettingsPath, strings.NewReader(body))
		w := httptest.NewRecorder()
		m.providerSettingsHTTP(w, r, "alice", route)
		if w.Code != 200 {
			t.Fatalf("%s: got HTTP %d", method, w.Code)
		}
		return w.Body.String()
	}
	if response := call(http.MethodGet, ""); !strings.Contains(response, "Fixture Search") {
		t.Fatal("additional provider did not appear in the settings catalog")
	}
	if response := call(http.MethodPut, `{"provider":"fixture","apiKey":"fixture-secret"}`); !strings.Contains(response, `"provider":"fixture"`) || strings.Contains(response, "fixture-secret") {
		t.Fatal("settings did not select the new provider and redact its key")
	}
	account, err := store.Get(t.Context(), "alice")
	if err != nil || account.WebSearch.Entries["deepseek"].APIKey != "legacy-search-key" {
		t.Fatal("adding a provider discarded the saved DeepSeek key")
	}
}

func TestSearchProviderSaveAndSelectAreSeparate(t *testing.T) {
	var applied []providersettings.Selection
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var change providersettings.Selection
		if r.URL.Path != "/internal/host/web-search" || r.Header.Get("Authorization") != "Bearer host-token" {
			t.Error("wrong Host endpoint or credential")
		}
		if err := json.NewDecoder(r.Body).Decode(&change); err != nil {
			t.Error(err)
		}
		applied = append(applied, change)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer host.Close()
	store := &memoryStore{records: map[string]Account{
		"alice": {DesiredState: running, HostEndpoint: host.URL, HostToken: "host-token", WebSearch: &providersettings.Settings{APIKey: "legacy-secret"}},
		"bob":   {},
	}}
	m := New(config.Default(), nil, store)
	call := func(user, method, suffix, body string, status int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, webSearchSettingsPath+suffix, strings.NewReader(body))
		r.Host = "127.0.0.1"
		r.Header.Set(UserHeader, user)
		w := httptest.NewRecorder()
		m.Handler().ServeHTTP(w, r)
		if w.Code != status || strings.Contains(w.Body.String(), "-secret") {
			t.Fatalf("%s: unexpected status or credential disclosure: %d", method, w.Code)
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	call("alice", "PUT", "", `{"provider":"doubao","activate":true}`, 400)
	got := call("alice", "PUT", "", `{"provider":"zhipu","apiKey":"zhipu-secret","activate":false}`, 200)
	if got["provider"] != "deepseek" || len(applied) != 0 {
		t.Fatal("saving an inactive key changed the selection or contacted the Host")
	}
	got = call("alice", "PUT", "", `{"provider":"zhipu","activate":true}`, 200)
	if got["provider"] != "zhipu" || len(applied) != 1 || applied[0].Provider != "zhipu" || applied[0].APIKey != "zhipu-secret" {
		t.Fatal("activation did not apply the saved Zhipu credentials")
	}
	restarted := New(config.Default(), nil, store)
	stored, err := restarted.Store.Get(t.Context(), "alice")
	if err != nil || stored.WebSearch.Provider != "zhipu" || stored.WebSearch.Entries["deepseek"].APIKey != "legacy-secret" {
		t.Fatal("provider selection or prior key was not persisted")
	}
	got = call("bob", "GET", "", "", 200)
	if got["provider"] != "deepseek" || len(got["entries"].(map[string]any)) != 0 {
		t.Fatal("settings were shared between accounts")
	}
	got = call("alice", "DELETE", "?provider=zhipu", "", 200)
	if got["provider"] != "zhipu" || len(applied) != 2 || applied[1].APIKey != "" {
		t.Fatal("removing the active key did not disable that provider on Host")
	}
}
