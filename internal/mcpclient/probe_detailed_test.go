package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	mcpconfig "go-bot/mcp"
)

func TestDetailedProbeKeepsRawNamesIdentityCapsAndLiveGeneration(t *testing.T) {
	manager, _, _ := managedFixture(t, "eager")
	scope := manager.NewScope()
	defer scope.Close()
	before := manager.Status()
	tool := findTool(t, scope, PublicToolName("fixture", "one"))
	for _, limit := range []struct {
		pages, names int64
		expected     []string
		truncated    bool
	}{
		{16, 512, []string{"one", "two"}, false},
		{1, 512, []string{"one"}, true},
		{16, 1, []string{"one"}, true},
		{16, 2, []string{"one", "two"}, false},
	} {
		result, err := manager.ProbeDetailed(t.Context(), "fixture", mcpconfig.ProbeOptions{MaxPages: &limit.pages, MaxToolNames: &limit.names})
		if err != nil || !result.OK || !result.CloseConfirmed || result.CleanupError != "" || result.Truncated != limit.truncated || !reflect.DeepEqual(result.ToolNames, limit.expected) {
			t.Fatalf("probe limits %+v: %+v %v", limit, result, err)
		}
		if result.ServerInfo == nil || result.ServerInfo.Name != "remote-name-is-not-our-namespace" || result.ServerInfo.Version != "1.0.0" {
			t.Fatalf("lost raw server identity: %+v", result)
		}
		data, _ := json.Marshal(result)
		if bytes.Contains(data, []byte(`"stage"`)) || bytes.Contains(data, []byte(`"message"`)) {
			t.Fatalf("success contains failure fields: %s", data)
		}
	}
	if !reflect.DeepEqual(before, manager.Status()) || tool != findTool(t, scope, PublicToolName("fixture", "one")) {
		t.Fatal("one-shot probe changed the active generation")
	}
}

func TestDetailedProbeEmptySuccessAndWaitingCredentialsAreRealOutcomes(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "empty", Version: "1"}, nil)
	remote := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	defer remote.Close()
	manager := NewManager(t.Context(), ManagerOptions{})
	defer manager.Close()
	disabled := false
	if _, err := manager.Apply(t.Context(), mcpconfig.Settings{Servers: map[string]mcpconfig.Server{
		"empty": {Enabled: &disabled, Transport: "streamable-http", URL: remote.URL},
		"key":   {Transport: "streamable-http", URL: remote.URL, Headers: map[string]string{"Authorization": "Bearer ${credential:missing}"}},
		"oauth": {Transport: "streamable-http", URL: remote.URL, OAuth: &mcpconfig.OAuth{ClientID: "example-client"}},
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := manager.ProbeDetailed(t.Context(), "empty", mcpconfig.ProbeOptions{})
	if err != nil || !result.OK || len(result.ToolNames) != 0 || result.Truncated || !result.CloseConfirmed {
		t.Fatalf("empty: %+v %v", result, err)
	}
	data, _ := json.Marshal(result)
	if !bytes.Contains(data, []byte(`"toolNames":[]`)) || !bytes.Contains(data, []byte(`"truncated":false`)) {
		t.Fatalf("empty success wire shape: %s", data)
	}
	for _, name := range []string{"key", "oauth"} {
		result, err := manager.ProbeDetailed(t.Context(), name, mcpconfig.ProbeOptions{})
		if err != nil || result.OK || result.Stage != "connect" || result.Message == "" || !result.CloseConfirmed {
			t.Fatalf("waiting %s: %+v %v", name, result, err)
		}
		data, _ := json.Marshal(result)
		if bytes.Contains(data, []byte(`"toolNames"`)) || bytes.Contains(data, []byte(`"serverInfo"`)) {
			t.Fatalf("failure wire shape: %s", data)
		}
	}
}

func TestDetailedProbeSeparatesConnectListTimeoutAndCancellation(t *testing.T) {
	for _, behavior := range []string{"connect-error", "list-error", "timeout", "cancel-list"} {
		t.Run(behavior, func(t *testing.T) {
			server := testServer()
			handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					data, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(bytes.NewReader(data))
					var request struct {
						Method string `json:"method"`
					}
					_ = json.Unmarshal(data, &request)
					if behavior == "connect-error" {
						http.Error(w, "connection fixture rejected", 500)
						return
					}
					if behavior == "timeout" {
						<-r.Context().Done()
						return
					}
					if request.Method == "tools/list" {
						if behavior == "list-error" {
							http.Error(w, "listing fixture rejected", 500)
							return
						}
						if behavior == "cancel-list" {
							cancel()
							<-r.Context().Done()
							return
						}
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer remote.Close()
			manager := NewManager(t.Context(), ManagerOptions{})
			defer manager.Close()
			disabled := false
			if _, err := manager.Apply(t.Context(), mcpconfig.Settings{Servers: map[string]mcpconfig.Server{"test": {Enabled: &disabled, Transport: "streamable-http", URL: remote.URL}}}); err != nil {
				t.Fatal(err)
			}
			options := mcpconfig.ProbeOptions{}
			if behavior == "timeout" {
				ms := float64(60)
				options.TimeoutMS = &ms
			}
			result, err := manager.ProbeDetailed(ctx, "test", options)
			stage := "connect"
			if behavior == "list-error" || behavior == "cancel-list" {
				stage = "list"
			}
			if err != nil || result.OK || result.Stage != stage || result.Message == "" || !result.CloseConfirmed || result.CleanupError != "" {
				t.Fatalf("%s: %+v %v", behavior, result, err)
			}
			if behavior == "timeout" && result.Message != "probe timed out" {
				t.Fatalf("timeout: %+v", result)
			}
			if behavior == "cancel-list" && result.Message != "probe aborted" {
				t.Fatalf("cancel: %+v", result)
			}
		})
	}
}

type probeCloseErrorTransport struct{ base sdk.Transport }

func (t probeCloseErrorTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	c, err := t.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return probeCloseErrorConnection{c}, nil
}

type probeCloseErrorConnection struct{ sdk.Connection }

func (c probeCloseErrorConnection) Close() error {
	return errors.Join(c.Connection.Close(), errors.New("probe cleanup fixture failed"))
}

func TestDetailedProbeKeepsSuccessAndCleanupFailureSeparateAndBlocksReplacement(t *testing.T) {
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	server := testServer()
	session, err := server.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	manager := NewManager(t.Context(), ManagerOptions{Connection: ConnectionOptions{Transport: probeCloseErrorTransport{clientTransport}}})
	defer manager.Close()
	disabled := false
	if _, err := manager.Apply(t.Context(), mcpconfig.Settings{Servers: map[string]mcpconfig.Server{"test": {Enabled: &disabled, Transport: "streamable-http", URL: "http://fixture.invalid/mcp"}}}); err != nil {
		t.Fatal(err)
	}
	result, err := manager.ProbeDetailed(t.Context(), "test", mcpconfig.ProbeOptions{})
	if err != nil || !result.OK || len(result.ToolNames) != 2 || !result.CloseConfirmed || !strings.Contains(result.CleanupError, "fixture failed") {
		t.Fatalf("cleanup overwrote probe outcome: %+v %v", result, err)
	}
	if _, err := manager.ProbeDetailed(t.Context(), "test", mcpconfig.ProbeOptions{}); err == nil {
		t.Fatal("cleanup failure allowed another probe")
	}
	if _, err := manager.Restart(t.Context(), "test"); err == nil {
		t.Fatal("cleanup failure allowed replacement")
	}
	if status := manager.Status(); len(status.Servers) != 1 || status.Servers[0].State != "failed" {
		t.Fatalf("cleanup diagnostic not retained: %+v", status)
	}
}

func TestDetailedProbeRejectsInvalidOptionsBeforeStartingWork(t *testing.T) {
	manager := NewManager(t.Context(), ManagerOptions{})
	defer manager.Close()
	zero := float64(0)
	negative := int64(-1)
	for _, options := range []mcpconfig.ProbeOptions{{TimeoutMS: &zero}, {MaxPages: &negative}, {MaxToolNames: &negative}} {
		_, err := manager.ProbeDetailed(t.Context(), "unknown", options)
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "MCP_INVALID_PROBE_OPTIONS" {
			t.Fatalf("invalid options reached server lookup: %v", err)
		}
	}
}

func TestDetailedProbeRedactsResolvedCredentialsInFailureValues(t *testing.T) {
	secret := "fixture-probe-secret-only"
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("probe did not receive its configured credential")
		}
		http.Error(w, "provider rejected "+secret, http.StatusInternalServerError)
	}))
	defer remote.Close()
	manager := NewManager(t.Context(), ManagerOptions{})
	defer manager.Close()
	disabled := false
	if _, err := manager.Apply(t.Context(), mcpconfig.Settings{Credentials: map[string]string{"probe/key": secret}, Servers: map[string]mcpconfig.Server{
		"private": {Enabled: &disabled, Transport: "streamable-http", URL: remote.URL, Headers: map[string]string{"Authorization": "Bearer ${credential:probe/key}"}},
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := manager.ProbeDetailed(t.Context(), "private", mcpconfig.ProbeOptions{})
	if err != nil || result.OK || result.Message == "" {
		t.Fatalf("expected a failure value: %+v %v", result, err)
	}
	data, _ := json.Marshal(result)
	if bytes.Contains(data, []byte(secret)) {
		t.Fatalf("probe exposed resolved credentials: %s", data)
	}
}
