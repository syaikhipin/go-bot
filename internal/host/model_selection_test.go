package host

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go-bot/harness/agent"
	"go-bot/internal/modelsettings"
	"go-bot/providers/model/mainmodel"
)

func TestHostExplicitModelSelectionUsesOnlyTheConfiguredEndpointAndProtocol(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/responses-api/responses" && input.Model == "second" && r.Header.Get("Authorization") == "Bearer responses-fixture" {
			_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responses selected"}]}]}`))
			return
		}
		if r.URL.Path == "/anthropic-api/messages" && input.Model == "third" && r.Header.Get("x-api-key") == "anthropic-fixture" {
			_, _ = w.Write([]byte(`{"id":"reply","type":"message","role":"assistant","content":[{"type":"text","text":"anthropic selected"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
			return
		}
		t.Error("selection reached a different endpoint, model or authentication mode", r.URL.Path, input.Model)
		http.Error(w, "wrong selection", 400)
	}))
	defer server.Close()
	models := hostModels{base: mainmodel.Model{Transport: server.Client().Transport}, snapshot: modelsettings.Snapshot{Catalog: modelsettings.Catalog{
		DefaultEndpointID: "responses",
		Endpoints: map[string]modelsettings.Endpoint{
			"responses": {BaseURL: server.URL + "/responses-api", APIKey: "responses-fixture", Protocol: modelsettings.Responses, Models: []string{"first", "second"}},
			"anthropic": {BaseURL: server.URL + "/anthropic-api", APIKey: "anthropic-fixture", Protocol: modelsettings.AnthropicMessages, Models: []string{"third"}},
		},
	}}}
	for _, row := range []struct{ provider, model, answer string }{{"responses", "second", "responses selected"}, {"anthropic", "third", "anthropic selected"}} {
		route := agent.ModelRoute{Provider: row.provider, Model: row.model}
		model, err := models.selected(t.Context(), route)
		if err != nil {
			t.Fatal(err)
		}
		result, err := model.Complete(agent.WithModelRoute(t.Context(), route), []agent.Message{{Role: "user", Content: "hello"}}, nil)
		if err != nil || result.Content != row.answer {
			t.Fatal("Host did not use the selected endpoint's actual adapter", result.Content, err)
		}
	}
	for _, route := range []agent.ModelRoute{{Provider: "unknown", Model: "second"}, {Provider: "responses", Model: "not-configured"}, {}} {
		if _, err := models.selected(t.Context(), route); err == nil {
			t.Fatal("an unavailable SDK route fell back to the default model", route)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("unavailable selection reached a remote service", calls.Load())
	}
}
