package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-bot/harness/assembly"
	"go-bot/harness/schedule"
	"go-bot/harness/session"
	"go-bot/internal/hostruntime"

	"github.com/google/uuid"
)

func TestHostFilesListsColdWorkspacesAndRejectsPathsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces", "cold-session")
	for _, dir := range []string{workspace, filepath.Join(root, ".dsh")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < maxWorkspaceListEntries+1; i++ {
		if err := os.WriteFile(filepath.Join(workspace, fmt.Sprintf("file-%d", i)), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	host := &hostruntime.NativeHost{Build: func(string, bool) (*hostruntime.NativeRuntime, error) {
		t.Fatal("directory listing activated a session")
		return nil, nil
	}}
	handler := NewNativeHostHTTP(host, NativeWorkerHTTPOptions{HostFilesRoot: root})
	call := func(method, query string, status int) workspaceFilePage {
		t.Helper()
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, httptest.NewRequest(method, "/v1/host/files"+query, nil))
		if out.Code != status {
			t.Fatalf("%s %s: got %d, want %d: %s", method, query, out.Code, status, out.Body)
		}
		var page workspaceFilePage
		if status == http.StatusOK {
			if err := json.Unmarshal(out.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
		}
		return page
	}
	page := call(http.MethodGet, "", http.StatusOK)
	if page.RootPath != root || page.Path != "" || len(page.Files) != 2 {
		t.Fatalf("root listing: %+v", page)
	}
	for _, file := range page.Files {
		if !file.IsDir || (file.Name != ".dsh" && file.Name != "workspaces") {
			t.Fatalf("unexpected root entry: %+v", file)
		}
	}
	page = call(http.MethodGet, "?path=workspaces/cold-session", http.StatusOK)
	if !page.Truncated || page.NextOffset != maxWorkspaceListEntries || len(page.Files) != maxWorkspaceListEntries {
		t.Fatalf("unexpected first page: %d files, offset %d", len(page.Files), page.NextOffset)
	}
	last := call(http.MethodGet, fmt.Sprintf("?path=workspaces/cold-session&offset=%d", page.NextOffset), http.StatusOK)
	if last.Truncated || len(last.Files) != 1 || last.Files[0].Size != 4 || !strings.HasPrefix(last.Files[0].Path, "workspaces/cold-session/") {
		t.Fatalf("unexpected final page: %+v", last)
	}
	call(http.MethodGet, "?path=%2e%2e", http.StatusBadRequest)
	call(http.MethodGet, "?path=%2Froot", http.StatusBadRequest)
	call(http.MethodGet, "?path=outside", http.StatusBadRequest)
	call(http.MethodGet, "?path=missing", http.StatusNotFound)
	call(http.MethodPost, "", http.StatusNotFound)
}

func TestPresentedFilePathDownloadsFromItsSessionWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "report #1+.txt"
	path := filepath.Join(workspace, name)
	outside := filepath.Join(root, "other.txt")
	for _, file := range []string{path, outside} {
		if err := os.WriteFile(file, []byte("report contents"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(workspace, "linked.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(sessionformat.PresentedFiles{Turn: 1, CallID: "present-report", Files: []sessionformat.PresentedFile{{Path: path}}})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	host := &hostruntime.NativeHost{
		ReadSession: func(context.Context, string) (sessionformat.PhysicalArtifact, error) {
			return sessionformat.PhysicalArtifact{Events: []sessionformat.Event{
				{Seq: 0, Type: "deliverables/presented", Data: data},
			}}, nil
		},
		WorkspaceForSession: func(sid string) string {
			if sid == id {
				return workspace
			}
			return ""
		},
	}
	handler := NewNativeHostHTTP(host, NativeWorkerHTTPOptions{})
	get := func(target string) *httptest.ResponseRecorder {
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, target, nil))
		return out
	}
	sessionPath := "/v1/sessions/" + id
	history := get(sessionPath + "/transcript")
	var page chatTranscriptPage
	if err := json.Unmarshal(history.Body.Bytes(), &page); err != nil || len(page.Events) != 1 {
		t.Fatalf("delivery history: %s (%v)", history.Body, err)
	}
	var message struct{ Message chatMessage }
	if err := json.Unmarshal(page.Events[0].Data, &message); err != nil || len(message.Message.Blocks) != 1 {
		t.Fatalf("delivery card: %s (%v)", page.Events[0].Data, err)
	}
	block := message.Message.Blocks[0]
	if block.Name != name || block.File == nil || block.File.Source != "presented" || block.File.Path != path {
		t.Fatalf("unexpected file block: %+v", block)
	}

	// A download resolves the workspace and reads the file without loading the
	// transcript, even when the Session is cold.
	host.ReadSession = func(context.Context, string) (sessionformat.PhysicalArtifact, error) {
		t.Fatal("file download read the Session journal")
		return sessionformat.PhysicalArtifact{}, nil
	}
	download := func(session, file string) *httptest.ResponseRecorder {
		return get(session + "/files/content?" + url.Values{"path": {file}}.Encode())
	}
	for _, file := range []string{block.File.Path, name} {
		out := download(sessionPath, file)
		if out.Code != http.StatusOK || out.Body.String() != "report contents" {
			t.Fatalf("download %q: got %d: %s", file, out.Code, out.Body)
		}
	}
	for _, file := range []string{outside, link, filepath.Join(workspace, "missing.txt")} {
		if out := download(sessionPath, file); out.Code != http.StatusNotFound {
			t.Fatalf("download %q: got %d, want 404", file, out.Code)
		}
	}
	if out := download("/v1/sessions/"+uuid.NewString(), path); out.Code != http.StatusNotFound {
		t.Fatalf("unknown Session: got %d, want 404", out.Code)
	}
}

func TestNativeHostHTTPScheduleHistoryReadsColdSessionWithoutActivation(t *testing.T) {
	ctx := context.Background()
	sessionID := uuid.NewString()
	record, err := schedule.NewAfter("schedule-history", "Check", "Original prompt", 3600, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	stamp := func(offset time.Duration) string { return now.Add(offset).Format("2006-01-02T15:04:05.000Z") }
	receipts := []schedule.Receipt{
		{ScheduledAt: stamp(-2 * time.Hour), DeliveredAt: stamp(-2*time.Hour + time.Second), MessageID: "message-one", Prompt: "Original prompt"},
		{ScheduledAt: stamp(-time.Hour), DeliveredAt: stamp(-time.Hour + time.Second), MessageID: "message-two", Prompt: "Original prompt"},
	}
	last := receipts[1]
	last.Prompt = ""
	store := schedule.FileStore{Path: filepath.Join(t.TempDir(), "schedule.json")}
	if err := store.Save(ctx, schedule.Document{Version: 1, Tasks: []schedule.Task{{
		SessionID: sessionID, Record: record, Status: "inactive", LastDelivery: &last,
		DeliveryHistory: schedule.History{Records: receipts},
	}}}); err != nil {
		t.Fatal(err)
	}
	service, err := schedule.NewHostService(store, func(context.Context, string) error { return nil }, func(context.Context, string, string) (string, error) {
		t.Fatal("history read activated delivery")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	builds := 0
	host := &hostruntime.NativeHost{UserID: "alice", Mode: "standalone", Schedule: service, Build: func(string, bool) (*hostruntime.NativeRuntime, error) {
		builds++
		return nil, errors.New("unexpected activation")
	}}
	handler := NewNativeHostHTTP(host, NativeWorkerHTTPOptions{})
	base := "/v1/sessions/" + sessionID + "/schedules/schedule-history/history?limit=1"
	read := func(path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response
	}
	first := read(base)
	var page schedule.HistoryPage
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &page) != nil || len(page.Records) != 1 ||
		page.Records[0].MessageID != "message-two" || page.NextBefore != "message-two" {
		t.Fatalf("first cold history page = %d %s", first.Code, first.Body.String())
	}
	second := read(base + "&before=" + page.NextBefore)
	page = schedule.HistoryPage{}
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &page) != nil || len(page.Records) != 1 ||
		page.Records[0].MessageID != "message-one" || page.NextBefore != "" {
		t.Fatalf("second cold history page = %d %s", second.Code, second.Body.String())
	}
	if response := read(strings.Replace(base, sessionID, uuid.NewString(), 1)); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "schedule_not_found") {
		t.Fatalf("foreign Session history = %d %s", response.Code, response.Body.String())
	}
	if response := read(base + "&before=unknown"); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "delivery_cursor_not_found") {
		t.Fatalf("invalid history cursor = %d %s", response.Code, response.Body.String())
	}
	if response := read(base + "&before="); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "delivery_cursor_not_found") {
		t.Fatalf("explicit empty history cursor = %d %s", response.Code, response.Body.String())
	}
	if response := read(strings.Replace(base, "limit=1", "limit=101", 1)); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid history limit = %d", response.Code)
	}
	if builds != 0 {
		t.Fatal("cold history read activated a Session")
	}
}

func TestNativeHostHTTPRejectsInvalidListCursorBeforeReadingHistory(t *testing.T) {
	listCalls := 0
	host := &hostruntime.NativeHost{UserID: "alice", Mode: "standalone", List: func(context.Context, string, int) (sessionformat.SessionListPage, error) {
		listCalls++
		return sessionformat.SessionListPage{}, nil
	}}
	handler := NewNativeHostHTTP(host, NativeWorkerHTTPOptions{})
	for _, path := range []string{
		"/v1/sessions?cursor=not-a-uuid",
		"/internal/host/sessions?cursor=not-a-uuid",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid Host list cursor %q = %d %s", path, response.Code, response.Body.String())
		}
	}
	if listCalls != 0 {
		t.Fatalf("invalid cursor reached history storage %d times", listCalls)
	}
}

func TestNativeHostHTTPRejectsUnknownSessionWithoutStartingIt(t *testing.T) {
	builds := 0
	host := &hostruntime.NativeHost{UserID: "alice", Mode: "standalone", Build: func(string, bool) (*hostruntime.NativeRuntime, error) {
		builds++
		return nil, errors.New("unavailable")
	}}
	handler := NewNativeHostHTTP(host, NativeWorkerHTTPOptions{})
	activity := httptest.NewRecorder()
	handler.ServeHTTP(activity, httptest.NewRequest(http.MethodGet, "/internal/host/activity", nil))
	if activity.Code != http.StatusOK {
		t.Fatalf("Host activity = %d %s", activity.Code, activity.Body.String())
	}
	removedReady := httptest.NewRecorder()
	handler.ServeHTTP(removedReady, httptest.NewRequest(http.MethodGet, "/internal/host/ready", nil))
	if removedReady.Code != http.StatusNotFound {
		t.Fatalf("retired Host ready endpoint = %d", removedReady.Code)
	}
	for _, path := range []string{
		"/v1/sessions/missing",
		"/v1/sessions/missing/events",
		"/internal/host/sessions/missing/runtime/activity",
		"/internal/runtime/runtime.status",
		"/internal/runtime/runtime.submit",
		"/internal/runtime/runtime.activate",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d", path, response.Code)
		}
	}
	if builds != 0 {
		t.Fatal("a public or runtime read implicitly created a Session")
	}
	for _, path := range []string{
		"/internal/host/sessions/new/activate",
		"/internal/host/sessions/new/stop",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusNotFound || builds != 0 {
			t.Fatalf("retired Host Session route %s = %d; build attempts = %d", path, response.Code, builds)
		}
	}
}

func TestHostSessionStatusHasNoRetiredTier(t *testing.T) {
	worker := &nativeHTTPWorker{done: make(chan struct{})}
	identity := assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: "new", Workspace: "/root/harness/workspaces/new"}
	response := httptest.NewRecorder()
	NewNativeWorkerHTTP(worker, identity, NativeWorkerHTTPOptions{}).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/v1/sessions/new", nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"tier"`) {
		t.Fatalf("new Host Session status = %d %s", response.Code, response.Body.String())
	}
}
