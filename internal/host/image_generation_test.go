package host

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-harness/internal/providersettings"
	"go-harness/providers/image/nanobanana"
	"go-harness/providers/image/openai"
)

func TestImageSettingsReplaceAndClearActiveGenerator(t *testing.T) {
	var host hostImageGeneration
	apply := func(body string, status int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/internal/host/image-generation", strings.NewReader(body))
		out := httptest.NewRecorder()
		host.handle(out, req)
		if out.Code != status {
			t.Fatalf("apply settings: got HTTP %d, want %d", out.Code, status)
		}
	}
	apply(`{"provider":"seedream","model":"ep-one","apiKey":"key-one"}`, 204)
	first := host.provider
	apply(`{"provider":"seedream","model":"ep-one","apiKey":"key-one"}`, 204)
	if first == nil || host.provider != first {
		t.Fatal("an unchanged selection replaced the generator")
	}
	apply(`{"provider":"seedream","model":"ep-two","apiKey":"key-one"}`, 204)
	if host.provider == nil || host.provider == first ||
		host.selection.Credential != (providersettings.Credential{Model: "ep-two", APIKey: "key-one"}) {
		t.Fatal("model edit did not replace the active generator")
	}
	apply(`{"provider":"seedream","model":"ep-two","apiKey":"key-two"}`, 204)
	apply(`{"provider":"unknown","model":"ep-two","apiKey":""}`, 400)
	if host.provider == nil || host.selection.APIKey != "key-two" {
		t.Fatal("invalid update changed the active generator")
	}
	apply(`{"provider":"seedream","model":"ep-two","apiKey":""}`, 204)
	_, _, err := host.Generate(t.Context(), "fixture", "a flower")
	if host.provider != nil || err == nil || !strings.Contains(err.Error(), "Settings → Image generation") {
		t.Fatal("cleared configuration did not return the settings hint")
	}
}

func TestImageSettingsSwitchLiveProviders(t *testing.T) {
	var host hostImageGeneration
	for _, provider := range []struct {
		id, model  string
		isExpected func(any) bool
	}{
		{openai.ID, openai.DefaultModel, func(v any) bool { _, ok := v.(*openai.Client); return ok }},
		{nanobanana.ID, nanobanana.DefaultModel, func(v any) bool { _, ok := v.(*nanobanana.Client); return ok }},
	} {
		body := `{"provider":"` + provider.id + `","model":"` + provider.model + `","apiKey":"fixture-key"}`
		req := httptest.NewRequest(http.MethodPut, "/internal/host/image-generation", strings.NewReader(body))
		out := httptest.NewRecorder()
		host.handle(out, req)
		if out.Code != 204 || !provider.isExpected(host.provider) {
			t.Fatalf("Host did not apply %s: HTTP %d", provider.id, out.Code)
		}
	}
}
