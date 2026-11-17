package standard

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go-bot/thirdparty/providerhttp"
	"go-bot/web"
)

func Count(request web.SearchRequest) int {
	if request.MaxResults < 1 {
		return 10
	}
	return min(request.MaxResults, 50)
}

func Source(address, title, snippet, summary, date string) (web.SearchSource, bool) {
	parsed, err := url.Parse(strings.TrimSpace(address))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return web.SearchSource{}, false
	}
	return web.SearchSource{URL: parsed.String(), Title: optional(title), Snippet: optional(snippet), Summary: optional(summary), PublishedAt: optional(date)}, true
}

func optional(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func Failed(provider string, status int, err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &web.Error{Code: "WEB_ABORTED", Message: provider + " search was interrupted or timed out.", Cause: err}
	case errors.Is(err, providerhttp.ErrResponse):
		return Invalid(provider)
	case err != nil:
		return &web.Error{Code: "WEB_PROVIDER_FAILED", Message: provider + " search could not reach the service."}
	}
	switch status {
	case 200:
		return nil
	case 401, 403:
		return &web.Error{Code: "WEB_PROVIDER_AUTH", Message: provider + " rejected the key or access. Check Web search settings."}
	case 402:
		return &web.Error{Code: "WEB_PROVIDER_QUOTA", Message: provider + " search quota or billing limit was reached."}
	case 429:
		return &web.Error{Code: "WEB_PROVIDER_RATE_LIMIT", Message: provider + " search is rate limited or its quota was reached."}
	default:
		return &web.Error{Code: "WEB_PROVIDER_FAILED", Message: fmt.Sprintf("%s search failed (HTTP %d).", provider, status)}
	}
}

func Invalid(provider string) error {
	return &web.Error{Code: "WEB_PROVIDER_FAILED", Message: provider + " returned an invalid or oversized search response."}
}
