package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	whatwg "github.com/nlnwa/whatwg-url/url"
	"go-bot/harness/agent"
	"go-bot/web"
)

type WebSearcher interface {
	Search(context.Context, web.SearchRequest) (web.SearchResult, error)
}

type WebSearch struct {
	Searcher     WebSearcher
	SpillWorkdir string
}

func (WebSearch) RetainsOutput() bool { return true }

const (
	webSearchMaxQueries = 10
	webSearchMaxResults = 10
	webSummaryMaxChars  = 800
)

func (WebSearch) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:         "web_search",
		Description:  "Search the web for current information. Returns an optional summary answer and a list of source URLs.",
		Parameters:   json.RawMessage(`{"type":"object","properties":{"queries":{"type":"array","items":{"type":"string"},"description":"1–10 search queries; their results are merged."}},"required":["queries"]}`),
		ParallelSafe: true,
		Timeout:      60 * time.Second,
	}
}

func (WebSearch) Validate(raw json.RawMessage) error {
	_, err := parseWebSearchArgs(raw)
	return err
}

func (t WebSearch) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	value, err := t.ExecuteValue(ctx, raw)
	if err != nil {
		return "", err
	}
	return formatWebSearchOutput(value), nil
}

func (t WebSearch) ExecutePresented(ctx context.Context, raw json.RawMessage) (string, json.RawMessage, error) {
	value, err := t.ExecuteValue(ctx, raw)
	if err != nil {
		return "", nil, err
	}
	sources := make([]web.SearchSource, 0, len(value.Sources))
	for _, source := range value.Sources {
		source.Summary = nil
		sources = append(sources, source)
	}
	meta, err := json.Marshal(struct {
		Sources   []web.SearchSource `json:"sources"`
		Truncated bool               `json:"truncated"`
		Answer    *string            `json:"answer,omitempty"`
	}{sources, value.Truncated, value.Content})
	text := retainWebText(ctx, t.SpillWorkdir, "web_search", formatWebSearchOutput(value))
	return text, meta, err
}

func (t WebSearch) ExecuteValue(ctx context.Context, raw json.RawMessage) (web.SearchResult, error) {
	queries, err := parseWebSearchArgs(raw)
	if err != nil {
		return web.SearchResult{}, err
	}
	if t.Searcher == nil {
		return web.SearchResult{}, &web.Error{Code: "WEB_PROVIDER_UNAVAILABLE", Message: "no usable web provider is registered"}
	}
	question := agent.ToolQuestionFromContext(ctx)
	search := func(ctx context.Context, query string) (web.SearchResult, error) {
		result, err := t.Searcher.Search(ctx, web.SearchRequest{Query: query, MaxResults: webSearchMaxResults, OriginalQuestion: question})
		if len(result.Sources) > webSearchMaxResults {
			result.Sources = result.Sources[:webSearchMaxResults]
			result.Truncated = true
		}
		return result, err
	}
	var result web.SearchResult
	if len(queries) == 1 {
		result, err = search(ctx, queries[0])
	} else {
		result, err = searchBatch(ctx, queries, search)
	}
	if err != nil {
		return web.SearchResult{}, err
	}
	sources := make([]web.SearchSource, 0, len(result.Sources))
	for _, source := range result.Sources {
		summary := web.CollapseSpace(stringValue(source.Summary))
		source.Summary = nil
		if summary != "" {
			if utf16Length(summary) > webSummaryMaxChars {
				summary = truncateUTF16(summary, webSummaryMaxChars-1) + "…"
			}
			source.Summary = &summary
		}
		sources = append(sources, source)
	}
	result.Sources = sources
	return result, nil
}

func searchBatch(ctx context.Context, queries []string, search func(context.Context, string) (web.SearchResult, error)) (web.SearchResult, error) {
	batchCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	results := make([]web.SearchResult, len(queries))
	var workers sync.WaitGroup
	var failure sync.Once
	var firstError error
	for index, query := range queries {
		workers.Go(func() {
			result, err := search(batchCtx, query)
			if err != nil {
				failure.Do(func() {
					firstError = err
					cancel(err)
				})
			}
			results[index] = result
		})
	}
	workers.Wait()
	if firstError != nil {
		return web.SearchResult{}, firstError
	}
	return mergeSearchResults(queries, results), nil
}

func mergeSearchResults(queries []string, results []web.SearchResult) web.SearchResult {
	var merged web.SearchResult
	var answers []string
	ranks := 0
	for index, result := range results {
		ranks = max(ranks, len(result.Sources))
		merged.Truncated = merged.Truncated || result.Truncated
		if answer := stringValue(result.Content); answer != "" {
			answers = append(answers, "### "+queries[index]+"\n\n"+answer)
		}
	}
	if len(answers) > 0 {
		answer := strings.Join(answers, "\n\n")
		merged.Content = &answer
	}
	seen := make(map[string]bool)
	for rank := 0; rank < ranks; rank++ {
		for _, result := range results {
			if rank >= len(result.Sources) {
				continue
			}
			source := result.Sources[rank]
			if seen[source.URL] {
				continue
			}
			if len(merged.Sources) == webSearchMaxResults {
				merged.Truncated = true
				return merged
			}
			seen[source.URL] = true
			merged.Sources = append(merged.Sources, source)
		}
	}
	return merged
}

func parseWebSearchArgs(raw json.RawMessage) ([]string, error) {
	var args struct {
		Queries []string `json:"queries"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if len(args.Queries) == 0 {
		return nil, errors.New("queries must contain at least one query")
	}
	if len(args.Queries) > webSearchMaxQueries {
		return nil, fmt.Errorf("queries must contain at most %d queries", webSearchMaxQueries)
	}
	seen := make(map[string]bool, len(args.Queries))
	queries := make([]string, 0, len(args.Queries))
	for _, query := range args.Queries {
		if strings.TrimFunc(query, web.IsSpace) == "" {
			return nil, errors.New("each query must be a non-empty string")
		}
		if !seen[query] {
			seen[query] = true
			queries = append(queries, query)
		}
	}
	return queries, nil
}

func formatWebSearchOutput(result web.SearchResult) string {
	parts := []string{webTrustNotice}
	answer := stringValue(result.Content)
	if answer != "" {
		parts = append(parts, answer)
	}
	if len(result.Sources) > 0 {
		lines := make([]string, 0, len(result.Sources))
		for _, source := range result.Sources {
			label := stringValue(source.Title)
			if label == "" {
				label = searchSourceLabel(source.URL)
			}
			line := "- [" + label + "](" + source.URL + ")"
			var detail []string
			if summary := stringValue(source.Summary); summary != "" {
				detail = append(detail, summary)
			} else if snippet := stringValue(source.Snippet); snippet != "" {
				detail = append(detail, snippet)
			}
			if date := stringValue(source.PublishedAt); date != "" {
				detail = append(detail, "("+date+")")
			}
			if len(detail) > 0 {
				line += " — " + strings.Join(detail, " ")
			}
			lines = append(lines, line)
		}
		parts = append(parts, "Sources:\n"+strings.Join(lines, "\n"))
	} else if answer == "" {
		parts = append(parts, "No results found.")
	}
	if result.Truncated {
		parts = append(parts, fmt.Sprintf("(Showing the first %d sources. Refine the query for more.)", len(result.Sources)))
	}
	parts = append(parts, "Cite the relevant URLs above as markdown links in your answer.")
	return strings.Join(parts, "\n\n")
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func searchSourceLabel(raw string) string {
	u, err := whatwg.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Hostname()
}
