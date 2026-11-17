package tools

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"go-bot/web"
)

type countingWebFetcher struct{ calls atomic.Int32 }

func (f *countingWebFetcher) Fetch(context.Context, string) (web.FetchResult, error) {
	f.calls.Add(1)
	return web.FetchResult{}, nil
}

type countingWebSearcher struct{ calls atomic.Int32 }

func (s *countingWebSearcher) Search(context.Context, web.SearchRequest) (web.SearchResult, error) {
	s.calls.Add(1)
	return web.SearchResult{}, nil
}

func TestGoWebBudgetsBoundConcurrentCalls(t *testing.T) {
	fetcher := &countingWebFetcher{}
	searcher := &countingWebSearcher{}
	boundedFetch := &BudgetedWebFetcher{Inner: fetcher, Max: 3}
	boundedSearch := &BudgetedWebSearcher{Inner: searcher, Max: 2}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = boundedFetch.Fetch(context.Background(), "https://example.com")
		}()
		go func() {
			defer wg.Done()
			_, _ = boundedSearch.Search(context.Background(), web.SearchRequest{Query: "query", MaxResults: 5})
		}()
	}
	wg.Wait()
	if got := fetcher.calls.Load(); got != 3 {
		t.Fatalf("web extraction provider calls = %d, want 3", got)
	}
	if got := searcher.calls.Load(); got != 2 {
		t.Fatalf("web search provider calls = %d, want 2", got)
	}
	if _, err := boundedFetch.Fetch(context.Background(), "https://example.com"); err == nil {
		t.Fatal("web extraction exceeded its budget")
	}
	if _, err := boundedSearch.Search(context.Background(), web.SearchRequest{Query: "query", MaxResults: 5}); err == nil {
		t.Fatal("web search exceeded its budget")
	}
}
