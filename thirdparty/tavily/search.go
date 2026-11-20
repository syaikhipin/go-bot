// Package tavily contains the wire contract for Tavily Search.
package tavily

import (
	"context"
	"net/http"

	"go-bot/thirdparty/providerhttp"
)

const Endpoint = "https://api.tavily.com/search"

type Result struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Content   string `json:"content"`
	Published string `json:"published_date"`
}

type Response struct {
	Results []Result `json:"results"`
}

func Search(ctx context.Context, client *http.Client, endpoint, key, query string, count int) (Response, int, error) {
	input := struct {
		Query          string `json:"query"`
		Count          int    `json:"max_results"`
		Depth          string `json:"search_depth"`
		Topic          string `json:"topic"`
		Answer         bool   `json:"include_answer"`
		RawContent     bool   `json:"include_raw_content"`
		Published      bool   `json:"include_published_date"`
		AutoParameters bool   `json:"auto_parameters"`
	}{Query: query, Count: count, Depth: "basic", Topic: "general", Published: true}
	var output Response
	status, err := providerhttp.Post(ctx, client, endpoint, "Authorization", "Bearer "+key, input, &output, 4<<20)
	return output, status, err
}
