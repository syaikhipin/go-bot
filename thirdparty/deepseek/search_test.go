package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go-harness/web"
)

func TestSearchUsesDedicatedEndpointAndStructuredResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/search" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("wrong endpoint or key")
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["need_content"] != false || req["query"] != "evidence" {
			t.Error("wrong request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"url":"https://example.com","title":"Evidence","snippet":"short","summary":"long","date":null}]}`))
	}))
	defer server.Close()
	c := Client{BaseURL: server.URL + "/v1/tools", APIKey: "test-key"}
	out, e := c.Search(context.Background(), "evidence", 10, "")
	if e != nil || len(out.Sources) != 1 || out.Sources[0].PublishedAt != nil {
		t.Fatalf("result: %+v %v", out, e)
	}
}
func TestSearchDoesNotFollowCredentialRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	_, e := (Client{BaseURL: source.URL, APIKey: "secret"}).Search(context.Background(), "q", 1, "")
	if e == nil || hits.Load() != 0 {
		t.Fatal("credential redirect followed")
	}
}

func TestSearchClassifiesProviderFailures(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        int
		body, message string
	}{
		{"unauthorized", 401, `{"error":{"message":"bad key"}}`, "DeepSeek API error (HTTP 401): bad key"},
		{"rate limited", 429, `not JSON`, "DeepSeek API error (HTTP 429)"},
		{"invalid source shape", 200, `{"results":[{"url":"https://example.com","title":"","snippet":""}]}`, "DeepSeek returned an invalid response body"},
		{"invalid date type", 200, `{"results":[{"url":"https://example.com","title":"","snippet":"","date":3}]}`, "DeepSeek returned an invalid response body"},
		{"invalid summary type", 200, `{"results":[{"url":"https://example.com","title":"","snippet":"","date":null,"summary":null}]}`, "DeepSeek returned an invalid response body"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer fixture.Close()
			client := Client{BaseURL: fixture.URL + "/v1/tools", APIKey: "fixture-search-key"}
			_, err := client.Search(t.Context(), "query", 10, "")
			var failure *web.Error
			if !errors.As(err, &failure) || failure.Code != "WEB_PROVIDER_ERROR" || !strings.HasPrefix(failure.Message, test.message+"\n\n") {
				t.Fatalf("provider failure: %v", err)
			}
		})
	}
}

func TestSearchCancellationIsTyped(t *testing.T) {
	ctx, stop := context.WithCancel(t.Context())
	stop()
	_, err := (Client{}).Search(ctx, "query", 10, "")
	var failure *web.Error
	if !errors.As(err, &failure) || failure.Code != "WEB_ABORTED" || !errors.Is(err, context.Canceled) {
		t.Fatalf("search cancellation: %v", err)
	}
}
