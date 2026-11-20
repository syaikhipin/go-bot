package controller

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"go-bot/internal/modelsettings"
)

func TestModelProtocolPersistsAndPublicViewHidesKey(t *testing.T) {
	manager := newTestManager(t, "docker", &fakeDriver{}, func(w http.ResponseWriter, r *http.Request) { controlReply(w, r) })
	if err := manager.CreateAccount(t.Context(), "protocol-test"); err != nil {
		t.Fatal(err)
	}
	out := controllerCall(manager, http.MethodPost, settingsPath+"/endpoints", "protocol-test", `{"name":"Anthropic","protocol":"anthropic_messages","baseUrl":"https://models.example/v1/","apiKey":"secret-should-not-leak","models":["custom-model"]}`)
	if out.Code != http.StatusCreated {
		t.Fatalf("%d %s", out.Code, out.Body)
	}
	var saved struct {
		EndpointID string `json:"endpointId"`
	}
	_ = json.Unmarshal(out.Body.Bytes(), &saved)
	state, err := manager.Store.LoadModels(t.Context(), "protocol-test")
	if err != nil || state.Catalog.Endpoints[saved.EndpointID].Protocol != modelsettings.AnthropicMessages {
		t.Fatalf("protocol not persisted: %v", err)
	}
	view := controllerCall(manager, http.MethodGet, settingsPath, "protocol-test", "")
	if !strings.Contains(view.Body.String(), `"protocol":"anthropic_messages"`) || strings.Contains(view.Body.String(), "secret-should-not-leak") {
		t.Fatal("public view lost protocol or exposed secret")
	}
	for _, body := range []string{`{"protocol":"guess"}`, `{"baseUrl":"https://models.example/v1/messages"}`} {
		bad := controllerCall(manager, http.MethodPatch, settingsPath+"/endpoints/"+saved.EndpointID, "protocol-test", body)
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid config: %d %s", bad.Code, bad.Body)
		}
	}
	defaultProtocol, err := (endpointPatch{}).apply(modelsettings.Endpoint{Name: "Existing", BaseURL: "https://models.example/v1", APIKey: "k", Models: []string{"model"}})
	if err != nil || defaultProtocol.Protocol != modelsettings.Responses {
		t.Fatal("existing Responses endpoint changed")
	}
}
