package zhipu

import (
	"context"
	"net/http"

	"go-harness/thirdparty/providerhttp"
)

const Endpoint = "https://open.bigmodel.cn/api/paas/v4/web_search"

type Result struct {
	Title     string `json:"title"`
	Link      string `json:"link"`
	Content   string `json:"content"`
	Published string `json:"publish_date"`
}

type Response struct {
	Results []Result `json:"search_result"`
	Error   *struct {
		Code string `json:"code"`
	} `json:"error"`
}

func Search(ctx context.Context, client *http.Client, endpoint, key, query string, count int) (Response, int, error) {
	req := struct {
		Query  string `json:"search_query"`
		Engine string `json:"search_engine"`
		Count  int    `json:"count"`
	}{query, "search_std", count}
	var response Response
	status, err := providerhttp.Post(ctx, client, endpoint, "Authorization", "Bearer "+key, req, &response, 4<<20)
	return response, status, err
}
