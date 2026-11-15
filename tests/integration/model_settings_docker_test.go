package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go-harness/internal/config"
	"go-harness/internal/modelsettings"
)

func TestDockerModelSettingsSwitchBetweenTurns(t *testing.T) {
	if os.Getenv("RUN_DOCKER_INTEGRATION") != "1" {
		t.Skip("requires the built Host image")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	type modelCall struct{ path, model, auth string }
	calls := make(chan modelCall, 8)
	release := make(chan struct{})
	model := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var input struct{ Model string }
		if err := json.Unmarshal(body, &input); err != nil {
			t.Error(err)
			return
		}
		calls <- modelCall{r.URL.Path, input.Model, r.Header.Get("Authorization")}
		output := []map[string]any{{"type": "message", "id": "reply", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": "Done"}}}}
		if input.Model == "first" && !strings.Contains(string(body), "function_call_output") {
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
			output = []map[string]any{webCall("tool", "shellexecute", `{"command":"printf model-switch"}`)}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		payload, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "reply", "status": "completed", "output": output, "usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
		}})
		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", payload)
	}))
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	model.Listener = listener
	model.Start()
	defer func() {
		cancel()
		model.Close()
	}()
	base := fmt.Sprintf("http://host.docker.internal:%d", listener.Addr().(*net.TCPAddr).Port)

	c := dockerFixtureConfig()
	c.Model.Main = config.MainModel{APIKey: "placeholder", BaseURL: base, Name: "placeholder"}
	c.Web.Provider, c.Web.APIKey = "deepseek", "fixture-search-key"
	manager := newFixtureManager(t, c)
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		if err := manager.Delete(cleanup); err != nil {
			t.Error(err)
		}
	}()
	if _, err := manager.Store.SaveModelCatalog(ctx, manager.account, modelsettings.Catalog{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(manager.Handler())
	defer server.Close()
	call := func(method, path, body string, status int) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != status {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("%s %s = %d: %s", method, path, resp.StatusCode, b)
		}
		return resp
	}
	decode := func(resp *http.Response, out any) {
		t.Helper()
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	call("POST", "/v1/host/start", "", 204).Body.Close()
	var failure map[string]string
	decode(call("POST", "/v1/sessions", "", 409), &failure)
	if failure["error"] != "model_default_required" {
		t.Fatalf("missing default error: %v", failure)
	}
	var empty struct{ Sessions []json.RawMessage }
	decode(call("GET", "/v1/sessions", "", 200), &empty)
	if len(empty.Sessions) != 0 {
		t.Fatal("created a session without a default model")
	}
	add := func(path, key string, models []string) string {
		t.Helper()
		body, _ := json.Marshal(modelsettings.Endpoint{Name: path, BaseURL: base + path, APIKey: key, Models: models})
		var endpoint struct{ EndpointID string }
		decode(call("POST", "/v1/settings/models/endpoints", string(body), 201), &endpoint)
		return endpoint.EndpointID
	}
	firstEndpoint := add("/first", "first-fixture-key", []string{"first", "alternative"})
	secondEndpoint := add("/second", "second-fixture-key", []string{"second"})
	settings := call("GET", "/v1/settings/models", "", 200)
	data, _ := io.ReadAll(settings.Body)
	settings.Body.Close()
	if strings.Contains(string(data), "fixture-key") || !strings.Contains(string(data), "hasApiKey") {
		t.Fatal("settings response did not redact keys")
	}
	create := func() string {
		t.Helper()
		var created struct{ SessionID string }
		decode(call("POST", "/v1/sessions", "", 201), &created)
		return created.SessionID
	}
	session := create()
	other := create()
	path := "/v1/sessions/" + session
	readSelection := func(sid string) modelsettings.Selection {
		t.Helper()
		var view struct{ Selection modelsettings.Selection }
		decode(call("GET", "/v1/sessions/"+sid+"/model", "", 200), &view)
		return view.Selection
	}
	first := modelsettings.Selection{EndpointID: firstEndpoint, ModelID: "first"}
	second := modelsettings.Selection{EndpointID: secondEndpoint, ModelID: "second"}
	if readSelection(session) != first || readSelection(other) != first {
		t.Fatal("sessions did not inherit the first model of the default endpoint")
	}
	defaultEndpoint, _ := json.Marshal(map[string]string{"endpointId": secondEndpoint})
	call("PUT", "/v1/settings/models/default", string(defaultEndpoint), 200).Body.Close()
	if readSelection(create()) != second || readSelection(session) != first {
		t.Fatal("changing the account default must only affect new sessions")
	}
	stream := call("GET", path+"/stream", "", 200)
	defer stream.Body.Close()
	call("POST", path+"/messages", `{"content":"Begin the first turn"}`, 202).Body.Close()
	expect := func(want modelCall) {
		t.Helper()
		select {
		case got := <-calls:
			if got != want {
				t.Fatalf("model call = %+v, want %+v", got, want)
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for the model")
		}
	}
	expect(modelCall{"/first/responses", "first", "Bearer first-fixture-key"})
	selection, _ := json.Marshal(second)
	call("PUT", path+"/model", string(selection), 200).Body.Close()
	close(release)
	expect(modelCall{"/first/responses", "first", "Bearer first-fixture-key"})
	events := readWebTurn(t, stream.Body)
	stream.Body.Close()
	stream = call("GET", fmt.Sprintf("%s/stream?after=%d", path, events[len(events)-1].Seq), "", 200)
	defer stream.Body.Close()
	call("POST", path+"/messages", `{"content":"Continue with the second model"}`, 202).Body.Close()
	expect(modelCall{"/second/responses", "second", "Bearer second-fixture-key"})
	readWebTurn(t, stream.Body)
	stream.Body.Close()
	if readSelection(other) != first {
		t.Fatal("changing one session changed another session")
	}
	alternative, _ := json.Marshal(modelsettings.Selection{EndpointID: firstEndpoint, ModelID: "alternative"})
	call("PUT", path+"/model", string(alternative), 200).Body.Close()
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := manager.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if readSelection(session).ModelID != "alternative" {
		t.Fatal("restarted Host did not restore the session selection")
	}
	call("POST", path+"/messages", `{"content":"Use the alternate model"}`, 202).Body.Close()
	expect(modelCall{"/first/responses", "alternative", "Bearer first-fixture-key"})
}
