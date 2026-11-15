package controller

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

func TestControllerServesWebRoutesAndLeavesAPIRoutesDistinct(t *testing.T) {
	files := fstest.MapFS{
		"index.html":     {Data: []byte("<!doctype html><title>Go Harness</title>")},
		"assets/main.js": {Data: []byte("const page = 'Go Harness'")},
	}
	handler := (&Manager{}).HandlerWithWeb(WebOptions{Files: files})
	for _, tc := range []struct {
		path, method, mime, cache string
		status                    int
	}{
		{"/", "GET", "text/html", "no-cache", 200},
		{"/chat/6c265cac-35e2-471a-80e1-45e066a909be", "GET", "text/html", "no-cache", 200},
		{"/", "HEAD", "text/html", "no-cache", 200},
		{"/assets/main.js", "GET", "javascript", "immutable", 200},
		{"/assets/no.js", "GET", "text/plain", "", 404},
		{"/chat/not-an-id", "GET", "text/plain", "", 404},
		{"/v1/unknown", "GET", "text/plain", "no-store", 404},
		{"/", "POST", "text/plain", "", 404},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(tc.method, "http://127.0.0.1:8080"+tc.path, nil)
		handler.ServeHTTP(w, r)
		if tc.mime == "text/html" && !slices.Contains(strings.Split(w.Header().Get("Content-Security-Policy"), "; "), "img-src 'self' blob:") {
			t.Errorf("%s %s: policy blocks chat image blob URLs", tc.method, tc.path)
		}
		if w.Code != tc.status || !strings.Contains(w.Header().Get("Content-Type"), tc.mime) ||
			!strings.Contains(w.Header().Get("Cache-Control"), tc.cache) || (tc.method == "HEAD" && w.Body.Len() != 0) {
			t.Errorf("%s %s: status %d headers %v body %q", tc.method, tc.path, w.Code, w.Header(), w.Body.String())
		}
	}
}

func TestWebDevOriginAndCrossOriginAPI(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1:5173", "http://[::1]:5173", "http://localhost:5173", ""} {
		if err := ValidateWebDevOrigin(origin); err != nil {
			t.Errorf("valid origin %q: %v", origin, err)
		}
	}
	for _, origin := range []string{"https://127.0.0.1:5173", "http://example.com:5173", "http://127.0.0.1", "http://127.0.0.1:5173/", "http://127.0.0.1:0"} {
		if err := ValidateWebDevOrigin(origin); err == nil {
			t.Errorf("invalid origin %q accepted", origin)
		}
	}
	handler := (&Manager{}).Handler()
	for _, r := range []*http.Request{
		httptest.NewRequest("POST", "http://127.0.0.1:8080/v1/sessions", nil),
		httptest.NewRequest("DELETE", "http://127.0.0.1:8080/v1/sessions/id", nil),
	} {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("foreign browser %s not rejected: %d", r.Method, w.Code)
		}
	}
}

func TestControllerBrowserOriginUsesRequestHostAndScheme(t *testing.T) {
	const origin = "https://computer.tailnet.ts.net"
	handler := (&Manager{}).HandlerWithWeb(WebOptions{DevOrigin: "http://localhost:5173"})
	for _, test := range []struct {
		name, host, origin, fetchSite, forwardedProto string
		status                                        int
	}{
		{"mobile", "computer.tailnet.ts.net", "", "", "https", 204},
		{"same origin browser", "computer.tailnet.ts.net:443", origin, "same-origin", "https", 204},
		{"another domain", "controller.example.org", "https://controller.example.org", "same-origin", "https", 204},
		{"direct HTTP domain", "lan.example.org:8080", "http://lan.example.org:8080", "same-origin", "", 204},
		{"loopback", "localhost:8080", "http://localhost:8080", "same-origin", "", 204},
		{"local development", "localhost:8080", "http://localhost:5173", "same-site", "", 204},
		{"other host", "foreign.tailnet.ts.net", origin, "", "https", 403},
		{"other port", "computer.tailnet.ts.net:8443", origin, "", "https", 403},
		{"foreign origin", "computer.tailnet.ts.net", "https://foreign.tailnet.ts.net", "same-site", "https", 403},
		{"http origin for https", "computer.tailnet.ts.net", "http://computer.tailnet.ts.net", "cross-site", "https", 403},
		{"development origin for remote host", "computer.tailnet.ts.net", "http://localhost:5173", "cross-site", "https", 403},
		{"cross site without origin", "computer.tailnet.ts.net", "", "cross-site", "https", 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1:8080/v1/sessions", nil)
			r.Host = test.host
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Sec-Fetch-Site", test.fetchSite)
			r.Header.Set("X-Forwarded-Host", "computer.tailnet.ts.net")
			r.Header.Set("X-Forwarded-Proto", test.forwardedProto)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("got %d, want %d: %s", w.Code, test.status, w.Body)
			}
		})
	}
}

func TestControllerRoutesHostFilesAndDescribesAccountStorage(t *testing.T) {
	for _, test := range []struct{ provider, storage, kind, source string }{
		{"docker", "container", "docker-volume", "gh-alice-data"},
		{"eci", "nas", "nas", "/go-harness/alice"},
		{"eci", "container", "instance", ""},
	} {
		t.Run(test.provider+"/"+test.storage, func(t *testing.T) {
			manager := newTestManager(t, test.provider, &fakeDriver{}, func(w http.ResponseWriter, r *http.Request) {
				if controlReply(w, r) {
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != hostFilesPath || r.URL.Query().Get("path") != ".dsh" ||
					r.URL.Query().Get("offset") != "200" || r.Header.Get(UserHeader) != "" ||
					!strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
					t.Errorf("unexpected forwarded request: %s %s", r.Method, r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"rootPath":"/root/harness","path":".dsh","files":[]}`))
			})
			manager.Config.Storage.Type = test.storage
			if err := manager.CreateAccount(t.Context(), "alice"); err != nil {
				t.Fatal(err)
			}
			if err := manager.Ensure(t.Context(), "alice"); err != nil {
				t.Fatal(err)
			}
			path := hostFilesPath + "?path=.dsh&offset=200"
			out := controllerCall(manager, http.MethodGet, path, "alice", "")
			if out.Code != http.StatusOK || out.Header().Get(hostStorageHeader) != test.kind ||
				out.Header().Get(hostStorageSourceHeader) != test.source || !strings.Contains(out.Body.String(), `"rootPath":"/root/harness"`) {
				t.Fatalf("status %d, headers %v, body %s", out.Code, out.Header(), out.Body)
			}
			if got := controllerCall(manager, http.MethodGet, path, "unknown", "").Code; got != http.StatusNotFound {
				t.Fatalf("unknown account status: %d", got)
			}
			if got := controllerCall(manager, http.MethodDelete, path, "alice", "").Code; got != http.StatusNotFound {
				t.Fatalf("write status: %d", got)
			}
		})
	}
}

func TestWebControllerRoutesChatRequestsToFakeECIInstance(t *testing.T) {
	const id = "6c265cac-35e2-471a-80e1-45e066a909be"
	const origin = "https://controller.example.org:8443"
	driver := &fakeDriver{provider: "eci"}
	var seenMu sync.Mutex
	var seen []string
	manager := newTestManager(t, "eci", driver, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("internal Host authorization missing")
		}
		if controlReply(w, r) {
			return
		}
		seenMu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		seenMu.Unlock()
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sessionId":"` + id + `"}`))
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("id: 5\nevent: session\ndata: {}\n\n"))
			w.(http.Flusher).Flush()
		}
	})
	if err := manager.CreateAccount(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if out := controllerCall(manager, "POST", "/v1/settings/models/endpoints", "alice", `{"name":"test","apiKey":"key","baseUrl":"https://models.example","models":["test"]}`); out.Code != 201 {
		t.Fatal(out.Body.String())
	}
	if err := manager.Ensure(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{"index.html": {Data: []byte("<title>Go Harness</title>")}}
	handler := manager.HandlerWithWeb(WebOptions{Files: files})
	for _, call := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", 200},
		{"POST", "/v1/sessions", 201},
		{"GET", "/v1/sessions/" + id + "/stream?after=4", 200},
		{"DELETE", "/v1/sessions/" + id, 204},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(call.method, "http://127.0.0.1:8080"+call.path, nil)
		req.Host = "controller.example.org:8443"
		req.Header.Set("Origin", origin)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set(UserHeader, "alice")
		handler.ServeHTTP(w, req)
		if w.Code != call.status {
			t.Fatalf("%s %s returned %d: %s", call.method, call.path, w.Code, w.Body.String())
		}
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(driver.specs) != 1 || !slices.Equal(seen, []string{
		"POST /internal/host/sessions", "GET /v1/sessions/" + id + "/stream?after=4", "DELETE /v1/sessions/" + id,
	}) {
		t.Fatalf("requests routed to fake ECI: %v, created=%d", seen, len(driver.specs))
	}
}
