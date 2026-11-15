package tools

import (
	"context"
	"encoding/json"
	"errors"
	"go-harness/web"
	"strings"
	"sync"
	"testing"
)

type batchSearchStub struct {
	mu      sync.Mutex
	results map[string]web.SearchResult
	calls   map[string]int
}

func (s *batchSearchStub) Search(_ context.Context, request web.SearchRequest) (web.SearchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[request.Query]++
	if request.MaxResults != webSearchMaxResults {
		return web.SearchResult{}, errors.New("wrong per-query source cap")
	}
	return s.results[request.Query], nil
}

func TestWebSearchMergesConcurrentQueriesByRankAndDeduplicatesURLs(t *testing.T) {
	source := func(name string) web.SearchSource {
		return web.SearchSource{Title: new(name), URL: "https://example.com/" + name, Snippet: new("snippet " + name)}
	}
	searcher := &batchSearchStub{results: map[string]web.SearchResult{
		"one": {Sources: []web.SearchSource{source("a"), source("shared"), source("c")}},
		"two": {Sources: []web.SearchSource{source("b"), source("shared"), source("d")}},
	}, calls: map[string]int{}}
	output, err := (WebSearch{Searcher: searcher}).Execute(context.Background(), json.RawMessage(`{"queries":["one","two","one"]}`))
	if err != nil || searcher.calls["one"] != 1 || searcher.calls["two"] != 1 || strings.Count(output, "[shared]") != 1 {
		t.Fatalf("batch search = %q, calls=%v, err=%v", output, searcher.calls, err)
	}
	previous := -1
	for _, name := range []string{"a", "b", "shared", "c", "d"} {
		position := strings.Index(output, "["+name+"]")
		if position <= previous {
			t.Fatalf("sources were not merged by rank: %q", output)
		}
		previous = position
	}
}

func TestWebSearchCapsMergedSourcesAndMarksTruncation(t *testing.T) {
	searcher := &batchSearchStub{results: make(map[string]web.SearchResult), calls: map[string]int{}}
	for _, query := range []string{"one", "two"} {
		result := web.SearchResult{}
		for rank := 0; rank < webSearchMaxResults; rank++ {
			result.Sources = append(result.Sources, web.SearchSource{
				Title: new(query + string(rune('0'+rank))), URL: "https://example.com/" + query + string(rune('0'+rank)),
			})
		}
		searcher.results[query] = result
	}
	output, err := (WebSearch{Searcher: searcher}).Execute(context.Background(), json.RawMessage(`{"queries":["one","two"]}`))
	if err != nil || strings.Count(output, "https://example.com/") != webSearchMaxResults ||
		!strings.Contains(output, "(Showing the first 10 sources. Refine the query for more.)") ||
		strings.Contains(output, "[one5]") {
		t.Fatalf("merged source cap = %q, %v", output, err)
	}
}

func TestWebSearchPreservesPerQueryTruncation(t *testing.T) {
	searcher := &batchSearchStub{results: map[string]web.SearchResult{
		"one": {
			Sources: []web.SearchSource{{Title: new("First"), URL: "https://example.com/first"}}, Truncated: true,
		},
		"two": {
			Sources: []web.SearchSource{{Title: new("Second"), URL: "https://example.com/second"}},
		},
	}, calls: map[string]int{}}
	for _, queries := range []string{`{"queries":["one"]}`, `{"queries":["one","two"]}`} {
		output, err := (WebSearch{Searcher: searcher}).Execute(context.Background(), json.RawMessage(queries))
		if err != nil || !strings.Contains(output, "(Showing the first ") || !strings.Contains(output, "Refine the query for more.") {
			t.Fatalf("per-query truncation lost: output=%q error=%v", output, err)
		}
	}
}

func TestWebSearchOnlyLabelsMergedQueryAnswers(t *testing.T) {
	searcher := &batchSearchStub{results: map[string]web.SearchResult{
		"one": {Content: new("answer one")},
		"two": {Content: new("answer two")},
	}, calls: map[string]int{}}
	single, err := (WebSearch{Searcher: searcher}).Execute(context.Background(), json.RawMessage(`{"queries":["one"]}`))
	if err != nil || !strings.Contains(single, "answer one") || strings.Contains(single, "### one") {
		t.Fatalf("single-query answer = %q, %v", single, err)
	}
	merged, err := (WebSearch{Searcher: searcher}).Execute(context.Background(), json.RawMessage(`{"queries":["one","two"]}`))
	if err != nil || !strings.Contains(merged, "### one\n\nanswer one") || !strings.Contains(merged, "### two\n\nanswer two") {
		t.Fatalf("merged-query answer = %q, %v", merged, err)
	}
}

type cancellingBatchSearcher struct {
	settled chan struct{}
}

func (s cancellingBatchSearcher) Search(ctx context.Context, request web.SearchRequest) (web.SearchResult, error) {
	if request.Query == "slow" {
		defer close(s.settled)
		<-ctx.Done()
		return web.SearchResult{}, ctx.Err()
	}
	return web.SearchResult{}, errors.New("provider failed")
}

func TestWebSearchCancelsAndWaitsForSiblingAfterFailure(t *testing.T) {
	searcher := cancellingBatchSearcher{settled: make(chan struct{})}
	_, err := (WebSearch{Searcher: searcher}).Execute(context.Background(), json.RawMessage(`{"queries":["slow","fail"]}`))
	if err == nil || !strings.Contains(err.Error(), "provider failed") {
		t.Fatalf("provider failure was lost: %v", err)
	}
	select {
	case <-searcher.settled:
	default:
		t.Fatal("search returned before its cancelled sibling settled")
	}
}
