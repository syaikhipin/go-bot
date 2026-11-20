package brave

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go-bot/internal/providersettings"
	"go-bot/providers/web/internal/standard"
	api "go-bot/thirdparty/brave"
	"go-bot/thirdparty/providerhttp"
	"go-bot/web"
)

const ID = "brave"
const name = "Brave Search"

type Provider struct {
	endpoint, key string
	http          *http.Client
}

func New(options web.Options) (web.SearchProvider, error) {
	if options.APIKey == "" || !providersettings.ValidAPIKey(options.APIKey) {
		return nil, errors.New("invalid Brave Search API key")
	}
	endpoint := options.BaseURL
	if endpoint == "" {
		endpoint = api.Endpoint
	}
	return &Provider{endpoint, options.APIKey, providerhttp.New(30 * time.Second)}, nil
}

func (*Provider) Name() string { return ID }

func (p *Provider) Search(ctx context.Context, request web.SearchRequest) (web.SearchResult, error) {
	query := web.CollapseSpace(request.Query)
	if query == "" || utf8.RuneCountInString(query) > 600 || len(strings.Fields(query)) > 75 {
		return web.SearchResult{}, &web.Error{Code: "WEB_INVALID_QUERY", Message: "Brave Search requires 1–600 characters and at most 75 words per query. Shorten the query and try again."}
	}
	count := standard.Count(request)
	response, status, err := api.Search(ctx, p.http, p.endpoint, p.key, query, count)
	if err := standard.Failed(name, status, err); err != nil {
		return web.SearchResult{}, err
	}
	if response.Grounding == nil || response.Grounding.Results == nil {
		return web.SearchResult{}, standard.Invalid(name)
	}
	result := web.SearchResult{Sources: make([]web.SearchSource, 0, min(count, len(response.Grounding.Results)))}
	for _, row := range response.Grounding.Results {
		metadata := response.Sources[row.URL]
		title := row.Title
		if strings.TrimSpace(title) == "" {
			title = metadata.Title
		}
		published := ""
		// age[3] is the ISO timestamp; age[1] is the absolute calendar date.
		// Relative ages and last-fetch timestamps are not publication dates.
		if len(metadata.Age) > 3 {
			published = metadata.Age[3]
		}
		if published == "" && len(metadata.Age) > 1 {
			published = metadata.Age[1]
		}
		if source, ok := standard.Source(row.URL, title, metadata.Description, strings.Join(row.Snippets, "\n\n"), published); ok {
			result.Sources = append(result.Sources, source)
			if len(result.Sources) == count {
				break
			}
		}
	}
	return result, nil
}
