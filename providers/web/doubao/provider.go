package doubao

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go-bot/internal/providersettings"
	"go-bot/providers/web/internal/standard"
	api "go-bot/thirdparty/doubao"
	"go-bot/thirdparty/providerhttp"
	"go-bot/web"
)

const ID = "doubao"
const name = "Doubao Search"

type Provider struct {
	endpoint, key string
	http          *http.Client
}

func New(options web.Options) (web.SearchProvider, error) {
	if options.APIKey == "" || !providersettings.ValidAPIKey(options.APIKey) {
		return nil, errors.New("invalid Doubao Search API key")
	}
	endpoint := options.BaseURL
	if endpoint == "" {
		endpoint = api.Endpoint
	}
	return &Provider{endpoint, options.APIKey, providerhttp.New(30 * time.Second)}, nil
}

func (*Provider) Name() string { return ID }

func (p *Provider) Search(ctx context.Context, request web.SearchRequest) (web.SearchResult, error) {
	count := standard.Count(request)
	response, status, err := api.Search(ctx, p.http, p.endpoint, p.key, request.Query, count)
	if response.Metadata.Error != nil && status == http.StatusOK {
		switch response.Metadata.Error.Code {
		case 10406:
			status = http.StatusPaymentRequired
		case 10409:
			status = http.StatusForbidden
		case 10400:
			status = http.StatusBadRequest
		default:
			status = http.StatusBadGateway
		}
	}
	if err := standard.Failed(name, status, err); err != nil {
		return web.SearchResult{}, err
	}
	if response.Result == nil {
		return web.SearchResult{}, standard.Invalid(name)
	}
	result := web.SearchResult{Sources: make([]web.SearchSource, 0, min(count, len(response.Result.Results)))}
	for _, row := range response.Result.Results {
		if source, ok := standard.Source(row.URL, row.Title, row.Snippet, row.Summary, row.Published); ok {
			result.Sources = append(result.Sources, source)
			if len(result.Sources) == count {
				break
			}
		}
	}
	return result, nil
}
