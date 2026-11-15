package doubao

import (
	"context"
	"net/http"

	"go-harness/thirdparty/providerhttp"
)

const Endpoint = "https://open.feedcoopapi.com/search_api/web_search"

type Result struct {
	Title     string `json:"Title"`
	URL       string `json:"Url"`
	Snippet   string `json:"Snippet"`
	Summary   string `json:"Summary"`
	Published string `json:"PublishTime"`
}

type Response struct {
	Metadata struct {
		Error *struct {
			Code int `json:"CodeN"`
		} `json:"Error"`
	} `json:"ResponseMetadata"`
	Result *struct {
		Results []Result `json:"WebResults"`
	} `json:"Result"`
}

func Search(ctx context.Context, client *http.Client, endpoint, key, query string, count int) (Response, int, error) {
	req := map[string]any{
		"Query": query, "SearchType": "web", "Count": count,
		"Filter": map[string]bool{"NeedUrl": true},
	}
	var response Response
	status, err := providerhttp.Post(ctx, client, endpoint, "Authorization", "Bearer "+key, req, &response, 4<<20)
	return response, status, err
}
