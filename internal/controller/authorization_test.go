package controller

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go-bot/extensions/catalog"
	"go-bot/internal/config"
	"go-bot/internal/httpapi"
	"go-bot/internal/plugins"
	"go-bot/plugin"
)

func TestControllerAuthorizationUsesHostFlowsWithoutSavingAnswersInRedis(t *testing.T) {
	main, err := filepath.Abs("../../extensions/cordis/src/main.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(main), "../node_modules/@deepseek-ai/cordis/package.json")); err != nil {
		t.Skip("Install the Cordis runtime dependencies")
	}
	extensions, err := plugins.NewManager(t.Context(), plugins.ManagerConfig{Directory: t.TempDir(), Runtime: plugins.RuntimeConfig{Main: main}, PackageMain: filepath.Join(filepath.Dir(main), "package-main.mjs")})
	if err != nil {
		t.Fatal(err)
	}
	defer extensions.Close()
	host := httptest.NewServer(httpapi.Authenticate("authorization-host-token", extensions.Handler()))
	defer host.Close()
	store := &memoryStore{records: map[string]Account{"alice": {DesiredState: running, HostToken: "authorization-host-token", HostEndpoint: host.URL}}}
	manager := New(config.Default(), nil, store)
	request := func(method, path string, input any, status int) json.RawMessage {
		t.Helper()
		body, _ := json.Marshal(input)
		r := httptest.NewRequest(method, path, strings.NewReader(string(body)))
		r.Header.Set(UserHeader, "alice")
		out := httptest.NewRecorder()
		manager.Handler().ServeHTTP(out, r)
		if out.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, out.Code, out.Body.String())
		}
		return out.Body.Bytes()
	}
	directory := t.TempDir()
	files := map[string]string{
		"package.json": `{"name":"@fixture/authorization","version":"1.0.0","type":"module","exports":"./index.mjs","dsh":{"manifestVersion":1,"bundle":{"patch":"./cordis.patch.yml"}},"goBot":{"api":"1","requires":{"authorization":"^1.0.0","credentials":"^1.0.0"}}}`,
		"index.mjs":    `export const inject=['host'];export async function apply(ctx){await ctx.host.authorization.register({key:'account',title:'Fixture account',methods:[{id:'key',label:'API key'}],async run(session){const key=await session.prompt({kind:'secret',message:'Service API key'});await session.commit({kind:'api-key',key})}})}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manager.pluginCatalog = []catalog.Entry{{CatalogEntry: plugin.CatalogEntry{ID: "fixture", Package: "@fixture/authorization", Version: "1.0.0"}, Source: plugin.Source{Type: "directory", Spec: directory}}}
	request("POST", plugin.SettingsPath+"/packages", map[string]string{"pluginId": "fixture"}, 200)
	request("PUT", plugin.SettingsPath+"/packages/%40fixture%2Fauthorization", map[string]bool{"enabled": true}, 200)

	listed := request("GET", plugin.AuthorizationPath, nil, 200)
	if !strings.Contains(string(listed), `"key":"fixture/account"`) || !strings.Contains(string(listed), `"configured":false`) {
		t.Fatal("Controller did not read the actual registered flow", string(listed))
	}
	var attempt struct {
		ID      string                `json:"id"`
		Status  string                `json:"status"`
		Prompts []struct{ ID string } `json:"prompts"`
	}
	if err := json.Unmarshal(request("POST", plugin.AuthorizationPath, plugin.AuthorizationStart{Key: "fixture/account"}, 202), &attempt); err != nil {
		t.Fatal("Real Host did not prompt the initiating surface", err)
	}
	readyDeadline := time.Now().Add(4 * time.Second)
	for len(attempt.Prompts) == 0 {
		if err := json.Unmarshal(request("GET", plugin.AuthorizationPath+"/"+attempt.ID, nil, 200), &attempt); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(readyDeadline) {
			t.Fatal("Host did not publish its pending question")
		}
		time.Sleep(5 * time.Millisecond)
	}
	request("POST", plugin.AuthorizationPath, plugin.AuthorizationStart{Key: "fixture/account"}, 409)
	request("GET", plugin.AuthorizationPath+"/00000000-0000-0000-0000-000000000000", nil, 404)
	secret := "private-authorization-answer"
	response := request("POST", plugin.AuthorizationPath+"/"+attempt.ID+"/answer", plugin.AuthorizationAnswer{PromptID: attempt.Prompts[0].ID, Answer: &secret}, 200)
	if strings.Contains(string(response), secret) {
		t.Fatal("Answer endpoint echoed the private value")
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		if err := json.Unmarshal(request("GET", plugin.AuthorizationPath+"/"+attempt.ID, nil, 200), &attempt); err != nil {
			t.Fatal(err)
		}
		if attempt.Status == "authorized" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Controller did not observe the real Go credential commit")
		}
		time.Sleep(5 * time.Millisecond)
	}
	listed = request("GET", plugin.AuthorizationPath, nil, 200)
	if strings.Contains(string(listed), secret) || !strings.Contains(string(listed), `"configured":true`) || strings.Contains(string(listed), "Service API key") {
		t.Fatal("Flow list leaked interaction or missed configuration", string(listed))
	}
	account, err := store.Get(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := json.Marshal(account)
	if strings.Contains(string(stored), secret) || strings.Contains(string(stored), attempt.ID) {
		t.Fatal("Controller persisted a Host authorization answer or transient handle")
	}
	request("POST", plugin.AuthorizationPath, map[string]any{"key": "fixture/account", "unexpected": true}, 400)
}

func TestControllerDisconnectsNativeAuthorizationWithoutCredentialPlaceholder(t *testing.T) {
	main, err := filepath.Abs("../../extensions/cordis/src/main.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(main), "../node_modules/@deepseek-ai/cordis/package.json")); err != nil {
		t.Skip("Install Cordis runtime dependencies")
	}
	directory := t.TempDir()
	for name, body := range map[string]string{
		"package.json": `{"name":"@fixture/native","version":"1.0.0","type":"module","exports":"./index.mjs","goBot":{"api":"1"}}`,
		"index.mjs":    `export const inject=['host'];export async function apply(ctx){let configured=false;await ctx.host.authorization.register({key:'account',title:'Native CLI account',methods:[{id:'login',label:'Connect'}],async run(){configured=true},async inspect(){return {configured,accountLabel:configured?'alice@example.com':''}},async disconnect(){configured=false}})}`,
	} {
		if err = os.WriteFile(filepath.Join(directory, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	extensions, err := plugins.NewManager(t.Context(), plugins.ManagerConfig{Directory: t.TempDir(), Runtime: plugins.RuntimeConfig{Main: main}, PackageMain: filepath.Join(filepath.Dir(main), "package-main.mjs")})
	if err != nil {
		t.Fatal(err)
	}
	defer extensions.Close()
	host := httptest.NewServer(httpapi.Authenticate("native-host", extensions.Handler()))
	defer host.Close()
	store := &memoryStore{records: map[string]Account{"alice": {DesiredState: running, HostToken: "native-host", HostEndpoint: host.URL}}}
	manager := New(config.Default(), nil, store)
	manager.pluginCatalog = []catalog.Entry{{CatalogEntry: plugin.CatalogEntry{ID: "native", Package: "@fixture/native", Version: "1.0.0"}, Source: plugin.Source{Type: "directory", Spec: directory}}}
	request := func(method, path string, input any, status int) json.RawMessage {
		t.Helper()
		body, _ := json.Marshal(input)
		req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
		req.Header.Set(UserHeader, "alice")
		out := httptest.NewRecorder()
		manager.Handler().ServeHTTP(out, req)
		if out.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, out.Code, out.Body.String())
		}
		return out.Body.Bytes()
	}
	request("POST", plugin.SettingsPath+"/packages", map[string]string{"pluginId": "native"}, 200)
	request("PUT", plugin.SettingsPath+"/packages/%40fixture%2Fnative", map[string]bool{"enabled": true}, 200)
	listed := request("GET", plugin.AuthorizationPath, nil, 200)
	if !strings.Contains(string(listed), `"canDisconnect":true`) || !strings.Contains(string(listed), `"configured":false`) {
		t.Fatal(string(listed))
	}
	var attempt struct{ ID, Status string }
	if err = json.Unmarshal(request("POST", plugin.AuthorizationPath, plugin.AuthorizationStart{Key: "native/account"}, 202), &attempt); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for attempt.Status != "authorized" {
		if time.Now().After(deadline) {
			t.Fatalf("native authorization did not settle: %+v", attempt)
		}
		if err = json.Unmarshal(request("GET", plugin.AuthorizationPath+"/"+attempt.ID, nil, 200), &attempt); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	listed = request("GET", plugin.AuthorizationPath, nil, 200)
	if !strings.Contains(string(listed), `"accountLabel":"alice@example.com"`) || !strings.Contains(string(listed), `"configured":true`) {
		t.Fatal(string(listed))
	}
	response := request("POST", plugin.AuthorizationPath+"/disconnect", map[string]string{"key": "native/account"}, 200)
	if !strings.Contains(string(response), `"configured":false`) {
		t.Fatal(string(response))
	}
	listed = request("GET", plugin.AuthorizationPath, nil, 200)
	if !strings.Contains(string(listed), `"configured":false`) {
		t.Fatal(string(listed))
	}
	request("POST", plugin.AuthorizationPath+"/disconnect", map[string]string{"key": "missing/account"}, 404)
}
