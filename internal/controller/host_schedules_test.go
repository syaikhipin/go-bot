package controller

import (
	"net/http"
	"strings"
	"testing"
)

func TestControllerRoutesHostSchedulesForRequestedAccount(t *testing.T) {
	manager := newTestManager(t, "docker", &fakeDriver{}, func(w http.ResponseWriter, r *http.Request) {
		if controlReply(w, r) {
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/v1/host/schedules" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("unexpected Host request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"tasks":[]}`))
	})
	if err := manager.CreateAccount(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Ensure(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	out := controllerCall(manager, http.MethodGet, "/v1/host/schedules", "alice", "")
	if out.Code != http.StatusOK || out.Body.String() != `{"tasks":[]}` {
		t.Fatalf("list response: %d %s", out.Code, out.Body)
	}
	out = controllerCall(manager, http.MethodGet, "/v1/host/schedules", "missing", "")
	if out.Code != http.StatusNotFound {
		t.Fatalf("missing account response: %d %s", out.Code, out.Body)
	}
}
