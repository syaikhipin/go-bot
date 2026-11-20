package webproviders

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go-bot/harness/tools"
	"go-bot/web"
)

const tavilyFixture = `{"results":[{"url":"javascript:invalid"},{"title":"Fixture","url":"https://example.com/report","content":"Relevant source text","published_date":"2026-11-18"},{"url":"https://example.com/overflow"}],"answer":"Do not forward generated answers","raw_content":"Do not forward raw HTML"}`
const braveFixture = `{"grounding":{"generic":[{"url":"javascript:invalid","snippets":[]},{"url":"https://example.com/report","snippets":["Relevant source text","Another passage"]},{"url":"https://example.com/overflow","snippets":[]}]},"sources":{"https://example.com/report":{"title":"Fixture","description":"Source description","age":["November 18, 2026","2026-11-18","today","2026-11-18T00:00:00Z"],"fetched_content_timestamp":1}}}`

func checkInternationalRequest(t *testing.T, id string, r *http.Request, count int) {
	t.Helper()
	var body map[string]any
	if r.Method != http.MethodPost || r.URL.Path != "/search" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil {
		t.Error("unexpected search wire request")
		return
	}
	if id == "tavily" {
		if r.Header.Get("Authorization") != "Bearer fixture-secret" || body["query"] != "fixture query" || body["max_results"] != float64(count) || body["search_depth"] != "basic" || body["topic"] != "general" || body["include_answer"] != false || body["include_raw_content"] != false || body["auto_parameters"] != false || body["include_published_date"] != true {
			t.Errorf("incorrect Tavily search defaults: %+v", body)
		}
	} else {
		if r.Header.Get("X-Subscription-Token") != "fixture-secret" || r.Header.Get("Authorization") != "" || body["q"] != "fixture query" || body["count"] != float64(count) || body["maximum_number_of_urls"] != float64(count) || body["maximum_number_of_tokens"] != float64(8192) || body["enable_source_metadata"] != true {
			t.Errorf("incorrect Brave LLM Context defaults: %+v", body)
		}
	}
	if strings.Contains(string(mustJSON(t, body)), "fixture-secret") {
		t.Error("key was put into the body")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestInternationalSearchRequestAndToolProjection(t *testing.T) {
	for _, tc := range []struct{ id, reply string }{{"tavily", tavilyFixture}, {"brave", braveFixture}} {
		t.Run(tc.id, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checkInternationalRequest(t, tc.id, r, 1)
				io.WriteString(w, tc.reply)
			}))
			defer server.Close()
			provider, err := New(tc.id, web.Options{BaseURL: server.URL + "/search", APIKey: "fixture-secret"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := provider.Search(t.Context(), web.SearchRequest{Query: " fixture\nquery ", MaxResults: 1})
			if err != nil || len(result.Sources) != 1 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			source := result.Sources[0]
			if source.Title == nil || *source.Title != "Fixture" || source.Summary == nil || !strings.HasPrefix(*source.Summary, "Relevant source text") || source.PublishedAt == nil || !strings.HasPrefix(*source.PublishedAt, "2026-11-18") || result.Content != nil {
				t.Fatalf("source normalization lost data: %+v", source)
			}
		})
		// The actual tool receives the same bounded, untrusted source projection
		// for either service, rather than a vendor-generated answer or raw HTML.
		t.Run(tc.id+"-tool", func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.reply) }))
			defer server.Close()
			p, _ := New(tc.id, web.Options{BaseURL: server.URL, APIKey: "fixture-secret"})
			text, _, err := (tools.WebSearch{Searcher: p}).ExecutePresented(t.Context(), json.RawMessage(`{"queries":["fixture query"]}`))
			if err != nil || !strings.Contains(text, "https://example.com/report") || !strings.Contains(text, "Relevant source text") || !strings.Contains(text, "untrusted") || strings.Contains(text, "Do not forward") {
				t.Fatalf("unexpected tool projection: %s (%v)", text, err)
			}
		})
	}
}

func TestInternationalSearchLimitsErrorsAndEmptyResults(t *testing.T) {
	for _, id := range []string{"tavily", "brave"} {
		empty := `{"results":[]}`
		if id == "brave" {
			empty = `{"grounding":{"generic":[]},"sources":{}}`
		}
		cases := []struct {
			status      int
			reply, code string
		}{
			{200, empty, ""}, {200, `{}`, "WEB_PROVIDER_FAILED"}, {200, `{"results":`, "WEB_PROVIDER_FAILED"},
			{401, `{"error":"fixture-secret"}`, "WEB_PROVIDER_AUTH"}, {403, `{"error":"fixture-secret"}`, "WEB_PROVIDER_AUTH"},
			{402, `{}`, "WEB_PROVIDER_QUOTA"}, {429, `{}`, "WEB_PROVIDER_RATE_LIMIT"}, {500, `{}`, "WEB_PROVIDER_FAILED"},
		}
		if id == "tavily" {
			cases = append(cases, struct {
				status      int
				reply, code string
			}{432, `{}`, "WEB_PROVIDER_QUOTA"}, struct {
				status      int
				reply, code string
			}{433, `{}`, "WEB_PROVIDER_QUOTA"}, struct {
				status      int
				reply, code string
			}{434, `{}`, "WEB_PROVIDER_RATE_LIMIT"})
		}
		for _, tc := range cases {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count := 50
				if id == "tavily" {
					count = 20
				}
				checkInternationalRequest(t, id, r, count)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.reply)
			}))
			p, err := New(id, web.Options{BaseURL: server.URL + "/search", APIKey: "fixture-secret"})
			if err != nil {
				server.Close()
				t.Fatal(err)
			}
			result, err := p.Search(t.Context(), web.SearchRequest{Query: "fixture query", MaxResults: 100})
			server.Close()
			if tc.code == "" {
				if err != nil || len(result.Sources) != 0 {
					t.Fatalf("%s: empty response=%+v err=%v", id, result, err)
				}
				continue
			}
			failure, ok := err.(*web.Error)
			if !ok || failure.Code != tc.code || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatalf("%s HTTP %d: error=%v", id, tc.status, err)
			}
		}
	}
}

func TestInternationalSearchCancellationRedirectAndSizeLimit(t *testing.T) {
	for _, id := range []string{"tavily", "brave"} {
		t.Run(id, func(t *testing.T) {
			for _, key := range []string{"", " bad", "bad\nkey", strings.Repeat("x", 4097)} {
				if _, err := New(id, web.Options{APIKey: key}); err == nil {
					t.Fatal("invalid key accepted")
				}
			}
			var followed atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1) }))
			defer target.Close()
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			}))
			defer redirect.Close()
			p, _ := New(id, web.Options{BaseURL: redirect.URL, APIKey: "fixture-secret"})
			if _, err := p.Search(t.Context(), web.SearchRequest{Query: "fixture"}); err == nil || followed.Load() != 0 {
				t.Fatal("credential redirect was followed")
			}
			large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat("x", (4<<20)+1)) }))
			defer large.Close()
			p, _ = New(id, web.Options{BaseURL: large.URL, APIKey: "fixture-secret"})
			if _, err := p.Search(t.Context(), web.SearchRequest{Query: "fixture"}); err == nil {
				t.Fatal("oversized response accepted")
			}
			started := make(chan struct{})
			slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				close(started)
				select {
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
				}
			}))
			defer slow.Close()
			p, _ = New(id, web.Options{BaseURL: slow.URL, APIKey: "fixture-secret"})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := p.Search(ctx, web.SearchRequest{Query: "fixture"}); result <- err }()
			<-started
			cancel()
			select {
			case err := <-result:
				if failure, ok := err.(*web.Error); !ok || failure.Code != "WEB_ABORTED" {
					t.Fatalf("cancellation=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("request did not cancel")
			}
		})
	}
}

func TestBraveRejectsOversizedQueriesBeforeSending(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	p, _ := New("brave", web.Options{BaseURL: server.URL, APIKey: "fixture-secret"})
	for _, query := range []string{"  ", strings.Repeat("界", 601), strings.Repeat("word ", 76)} {
		_, err := p.Search(t.Context(), web.SearchRequest{Query: query})
		if failure, ok := err.(*web.Error); !ok || failure.Code != "WEB_INVALID_QUERY" {
			t.Fatalf("invalid query=%v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid query reached upstream")
	}
}
