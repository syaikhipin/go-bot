// Package web defines the contracts shared by web tools and their providers.
package web

import "context"

type SearchRequest struct {
	Query            string
	MaxResults       int
	OriginalQuestion string
}

type SearchProvider interface {
	Name() string
	Search(context.Context, SearchRequest) (SearchResult, error)
}

type SearchResult struct {
	Content   *string        `json:"content,omitempty"`
	Sources   []SearchSource `json:"sources"`
	Truncated bool           `json:"truncated"`
}

type SearchSource struct {
	URL         string  `json:"url"`
	Title       *string `json:"title,omitempty"`
	Snippet     *string `json:"snippet,omitempty"`
	Summary     *string `json:"summary,omitempty"`
	PublishedAt *string `json:"publishedAt,omitempty"`
}

type FetchBody struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

type FetchResult struct {
	URL        string    `json:"url"`
	StatusCode int       `json:"statusCode"`
	Body       FetchBody `json:"body"`
	Truncated  bool      `json:"truncated"`
}

type Error struct {
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string                   { return e.Message }
func (e *Error) Unwrap() error                   { return e.Cause }
func (e *Error) ToolErrorInfo() (string, string) { return "WebError", e.Code }

type Factory func(Options) (SearchProvider, error)
type Options struct{ BaseURL, APIKey string }

// Registry is explicit: installing a provider never changes the chosen provider.
type Registry map[string]Factory
