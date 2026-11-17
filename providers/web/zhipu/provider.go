package zhipu

import (
	"context"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"go-bot/internal/providersettings"
	"go-bot/providers/web/internal/standard"
	"go-bot/thirdparty/providerhttp"
	api "go-bot/thirdparty/zhipu"
	"go-bot/web"
)

const ID = "zhipu"
const name = "Zhipu Search"

type Provider struct {
	endpoint, key string
	http          *http.Client
}

func New(options web.Options) (web.SearchProvider, error) {
	if options.APIKey == "" || !providersettings.ValidAPIKey(options.APIKey) {
		return nil, errors.New("invalid Zhipu Search API key")
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
	if query == "" || utf8.RuneCountInString(query) > 70 {
		return web.SearchResult{}, &web.Error{Code: "WEB_INVALID_QUERY", Message: "Zhipu Search requires 1–70 characters per query. Shorten the query and try again."}
	}
	count := standard.Count(request)
	response, status, err := api.Search(ctx, p.http, p.endpoint, p.key, query, count)
	if response.Error != nil && status == http.StatusOK {
		if response.Error.Code == "1701" {
			status = http.StatusTooManyRequests
		} else {
			return web.SearchResult{}, standard.Invalid(name)
		}
	}
	if err := standard.Failed(name, status, err); err != nil {
		return web.SearchResult{}, err
	}
	if response.Results == nil {
		return web.SearchResult{}, standard.Invalid(name)
	}
	result := web.SearchResult{Sources: make([]web.SearchSource, 0, min(count, len(response.Results)))}
	for _, row := range response.Results {
		if source, ok := standard.Source(row.Link, row.Title, "", row.Content, row.Published); ok {
			result.Sources = append(result.Sources, source)
			if len(result.Sources) == count {
				break
			}
		}
	}
	return result, nil
}
