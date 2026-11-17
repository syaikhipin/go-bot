package webproviders

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-bot/harness/tools"
	"go-bot/web"
)

func TestDeepSeekProviderUsesSearchAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tools/search" || r.Header.Get("Authorization") != "Bearer search-fixture" {
			t.Errorf("unexpected search request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request struct {
			Query string `json:"query"`
			Count int    `json:"count"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Query != "fixture" || request.Count != 3 {
			t.Error("unexpected search arguments")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"title":"Fixture","url":"https://example.com","snippet":"Source","date":null}]}`))
	}))
	defer server.Close()
	provider, err := New("deepseek", web.Options{BaseURL: server.URL + "/v1/tools", APIKey: "search-fixture"})
	if err != nil || provider.Name() != "deepseek" {
		t.Fatalf("construct DeepSeek search provider: %v", err)
	}
	result, err := provider.Search(t.Context(), web.SearchRequest{Query: "fixture", MaxResults: 3})
	if err != nil || len(result.Sources) != 1 || result.Sources[0].Title == nil || *result.Sources[0].Title != "Fixture" {
		t.Fatalf("search did not return the fixture source: %v", err)
	}
	for _, id := range []string{"", "deepseek-search-api", "unknown"} {
		if provider, err := New(id, web.Options{}); err == nil || provider != nil {
			t.Fatalf("unregistered provider %q selected a backend", id)
		}
	}
}

type registeredFixture struct{ result web.SearchResult }

func (registeredFixture) Name() string { return "test-fixture" }
func (p registeredFixture) Search(context.Context, web.SearchRequest) (web.SearchResult, error) {
	return p.result, nil
}

func TestToolProjectionUsesSelectedProviderContract(t *testing.T) {
	const body = `{"results":[{"title":"Source","url":"https://example.com","snippet":"short","summary":"long","date":null}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body)
	}))
	defer server.Close()
	registry["test-fixture"] = adapter{name: "Test fixture", newProvider: func(web.Options) (web.SearchProvider, error) {
		return registeredFixture{web.SearchResult{Sources: []web.SearchSource{{
			URL: "https://example.com", Title: new("Source"), Snippet: new("short"), Summary: new("long"),
		}}}}, nil
	}}
	t.Cleanup(func() { delete(registry, "test-fixture") })
	args := json.RawMessage(`{"queries":["fixture"]}`)
	var output string
	var metadata json.RawMessage
	for _, id := range []string{"deepseek", "test-fixture"} {
		provider, err := New(id, web.Options{BaseURL: server.URL, APIKey: "fixture-key"})
		if err != nil {
			t.Fatal(err)
		}
		text, meta, err := (tools.WebSearch{Searcher: provider}).ExecutePresented(t.Context(), args)
		if err != nil {
			t.Fatal(err)
		}
		if id == "deepseek" {
			output, metadata = text, meta
			continue
		}
		if text != output || !bytes.Equal(meta, metadata) {
			t.Fatal("changing provider changed the normalized web tool projection")
		}
	}
}

func TestOfficialSearchProviders(t *testing.T) {
	for _, tc := range []struct {
		id, reply, empty, upstreamError, upstreamCode string
		keys                                          map[string]any
	}{
		{"zhipu", `{"search_result":[{"link":"javascript:invalid"},{"title":"Fixture","link":"https://example.com/z","content":"Summary","publish_date":"2026-11-01"},{"title":"Overflow","link":"https://example.com/other"}]}`, `{"search_result":[]}`, `{"error":{"code":"1701","message":"fixture-key"}}`, "WEB_PROVIDER_RATE_LIMIT", map[string]any{"search_query": "检索", "search_engine": "search_std", "count": float64(1)}},
		{"doubao", `{"ResponseMetadata":{},"Result":{"ResultCount":1,"WebResults":[{"Title":"Fixture","Url":"https://example.com/d","Snippet":"Short","Summary":"Summary","PublishTime":"2026-11-01"}]}}`, `{"ResponseMetadata":{},"Result":{"ResultCount":0,"WebResults":null}}`, `{"ResponseMetadata":{"Error":{"CodeN":10406,"Message":"fixture-key"}},"Result":null}`, "WEB_PROVIDER_QUOTA", map[string]any{"Query": "检索", "SearchType": "web", "Count": float64(1)}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			for _, entry := range []struct {
				body   string
				status int
				code   string
				count  int
			}{
				{tc.reply, 200, "", 1}, {tc.empty, 200, "", 0}, {tc.upstreamError, 200, tc.upstreamCode, 0},
				{`{"secret":"fixture-key"}`, 401, "WEB_PROVIDER_AUTH", 0}, {`{"bad":`, 200, "WEB_PROVIDER_FAILED", 0},
			} {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Method != http.MethodPost || r.URL.Path != "/search" || r.Header.Get("Authorization") != "Bearer fixture-key" {
						t.Error("unexpected search request")
					}
					for key, value := range tc.keys {
						if body[key] != value {
							t.Errorf("unexpected value for %s: %v", key, body[key])
						}
					}
					if tc.id == "doubao" {
						filter, ok := body["Filter"].(map[string]any)
						if !ok || filter["NeedUrl"] != true {
							t.Error("Doubao request must ask for source URLs")
						}
					}
					w.WriteHeader(entry.status)
					_, _ = io.WriteString(w, entry.body)
				}))
				p, err := New(tc.id, web.Options{BaseURL: server.URL + "/search", APIKey: "fixture-key"})
				if err != nil {
					server.Close()
					t.Fatal(err)
				}
				res, err := p.Search(t.Context(), web.SearchRequest{Query: "检索", MaxResults: 1})
				server.Close()
				if entry.code != "" {
					typed, ok := err.(*web.Error)
					if !ok || typed.Code != entry.code || strings.Contains(typed.Message, "fixture-key") {
						t.Fatalf("unexpected search error: %v", err)
					}
					continue
				}
				if err != nil || len(res.Sources) != entry.count {
					t.Fatalf("unexpected search result: %+v, %v", res, err)
				}
				if entry.count > 0 {
					source := res.Sources[0]
					if source.Title == nil || *source.Title != "Fixture" || source.Summary == nil || *source.Summary != "Summary" || source.PublishedAt == nil {
						t.Fatalf("source metadata was lost: %+v", source)
					}
				}
			}
		})
	}
}
