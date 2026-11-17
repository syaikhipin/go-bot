package tools

import (
	"context"
	"errors"
	"sync"

	"go-bot/web"
)

// These budgets belong to one live Go Assembly. Container web calls are
// bounded at the controller IPC boundary instead.
type BudgetedWebFetcher struct {
	Inner WebFetcher
	Max   int
	mu    sync.Mutex
	calls int
}

func (f *BudgetedWebFetcher) Fetch(ctx context.Context, url string) (web.FetchResult, error) {
	f.mu.Lock()
	if f.Inner == nil || f.Max <= 0 || f.calls >= f.Max {
		f.mu.Unlock()
		return web.FetchResult{}, errors.New("web extraction budget exhausted or unavailable")
	}
	f.calls++
	f.mu.Unlock()
	return f.Inner.Fetch(ctx, url)
}

type BudgetedWebSearcher struct {
	Inner WebSearcher
	Max   int
	mu    sync.Mutex
	calls int
}

func (s *BudgetedWebSearcher) Search(ctx context.Context, request web.SearchRequest) (web.SearchResult, error) {
	s.mu.Lock()
	if s.Inner == nil || s.Max <= 0 || s.calls >= s.Max {
		s.mu.Unlock()
		return web.SearchResult{}, errors.New("web search budget exhausted or unavailable")
	}
	s.calls++
	s.mu.Unlock()
	return s.Inner.Search(ctx, request)
}
