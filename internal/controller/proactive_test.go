package controller

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	proactive "go-bot/internal/proactivesettings"
)

func TestProactiveSettingsDoNotStartHostAndSurviveHostRecordWrites(t *testing.T) {
	driver := &fakeDriver{}
	manager := newTestManager(t, "docker", driver, func(w http.ResponseWriter, r *http.Request) { controlReply(w, r) })
	if err := manager.CreateAccount(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	response := controllerCall(manager, "GET", proactive.SettingsPath, "alice", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"enabled":true`) || !strings.Contains(response.Body.String(), `"phase":"host-stopped"`) || len(driver.specs) != 0 {
		t.Fatalf("read=%d %s", response.Code, response.Body.String())
	}
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enabled":"true"}`, `{"enabled":false,"runNow":true}`, `{"enabled":true} {}`} {
		if response := controllerCall(manager, "PUT", proactive.SettingsPath, "alice", body); response.Code != 400 {
			t.Fatalf("accepted %s: %d", body, response.Code)
		}
	}
	response = controllerCall(manager, "PUT", proactive.SettingsPath, "alice", `{"enabled":false}`)
	if response.Code != 200 || len(driver.specs) != 0 {
		t.Fatal(response.Body.String())
	}
	account, _ := manager.Store.Get(t.Context(), "alice")
	if account.Proactive == nil || account.Proactive.Enabled {
		t.Fatal("disabled preference not saved")
	}
	account.Proactive = nil // Host lifecycle saves cannot overwrite user settings.
	account.HostID = "new-instance"
	if err := manager.Store.Save(t.Context(), "alice", account); err != nil {
		t.Fatal(err)
	}
	restored, _ := manager.Store.Get(t.Context(), "alice")
	if restored.Proactive == nil || restored.Proactive.Enabled {
		t.Fatal("Host save lost the account preference")
	}
	if response := controllerCall(manager, "GET", proactive.SettingsPath, "missing", ""); response.Code != 404 {
		t.Fatalf("missing account: %d", response.Code)
	}
}

func TestControllerSynchronizesAccountPreferenceAndReturnsHostStatus(t *testing.T) {
	var mu sync.Mutex
	var received struct {
		Enabled       bool
		MainSessionID string
	}
	failApply := false
	manager := newTestManager(t, "docker", &fakeDriver{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == proactive.InternalPath {
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodPut {
				_ = json.NewDecoder(r.Body).Decode(&received)
				if failApply {
					w.WriteHeader(503)
				} else {
					w.WriteHeader(204)
				}
			} else {
				_ = json.NewEncoder(w).Encode(proactive.Status{Enabled: received.Enabled, Phase: "waiting", NextCheckAt: 1_795_300_000_123, NextCheckReason: "Read tomorrow's publication"})
			}
			return
		}
		controlReply(w, r)
	})
	if err := manager.CreateAccount(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Ensure(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	account, _ := manager.Store.Get(t.Context(), "alice")
	mu.Lock()
	initial := received
	mu.Unlock()
	if !initial.Enabled || initial.MainSessionID != account.MainSessionID {
		t.Fatalf("sync=%+v", initial)
	}
	response := controllerCall(manager, "PUT", proactive.SettingsPath, "alice", `{"enabled":false}`)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"enabled":false`) || !strings.Contains(response.Body.String(), `"nextCheckAt":1795300000123`) {
		t.Fatalf("status=%d %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"hostToken", "hostEndpoint", "mainSessionId", "sessionId"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("internal data leaked: %s", secret)
		}
	}
	mu.Lock()
	failApply = true
	mu.Unlock()
	response = controllerCall(manager, "PUT", proactive.SettingsPath, "alice", `{"enabled":true}`)
	if response.Code != 503 || !strings.Contains(response.Body.String(), settingsSavedHostApplyFailed) {
		t.Fatalf("apply failure=%d %s", response.Code, response.Body.String())
	}
	account, _ = manager.Store.Get(t.Context(), "alice")
	if account.Proactive == nil || !account.Proactive.Enabled {
		t.Fatal("explicit saved-setting error hid the Redis commit")
	}
}
