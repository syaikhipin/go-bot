// Package brave contains the wire contract for Brave's LLM Context Search API.
package brave

import (
	"context"
	"net/http"

	"go-bot/thirdparty/providerhttp"
)

const Endpoint = "https://api.search.brave.com/res/v1/llm/context"

type Result struct {
	URL      string   `json:"url"`
	Title    string   `json:"title"`
	Snippets []string `json:"snippets"`
}

type Source struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Age         []string `json:"age"`
}

type Response struct {
	Grounding *struct {
		Results []Result `json:"generic"`
	} `json:"grounding"`
	Sources map[string]Source `json:"sources"`
}

func Search(ctx context.Context, client *http.Client, endpoint, key, query string, count int) (Response, int, error) {
	input := struct {
		Query          string `json:"q"`
		Count          int    `json:"count"`
		URLs           int    `json:"maximum_number_of_urls"`
		Tokens         int    `json:"maximum_number_of_tokens"`
		TokensPerURL   int    `json:"maximum_number_of_tokens_per_url"`
		SourceMetadata bool   `json:"enable_source_metadata"`
	}{query, count, count, 8192, 1024, true}
	var output Response
	status, err := providerhttp.Post(ctx, client, endpoint, "X-Subscription-Token", key, input, &output, 4<<20)
	return output, status, err
}
