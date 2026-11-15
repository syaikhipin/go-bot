// Package deepseek wraps the dedicated DeepSeek Search API.
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"go-harness/web"
)

type Client struct {
	BaseURL, APIKey string
	HTTP            *http.Client
}

func (c Client) Search(ctx context.Context, query string, count int, question string) (web.SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return web.SearchResult{}, aborted(err)
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/search"
	if c.APIKey == "" || count < 1 || count > 50 {
		return web.SearchResult{}, &web.Error{Code: "WEB_PROVIDER_MISCONFIGURED", Message: "DeepSeek Search requires a search key and a count from 1 to 50"}
	}
	body, _ := json.Marshal(struct {
		Query       string `json:"query"`
		Count       int    `json:"count"`
		NeedContent bool   `json:"need_content"`
		Question    string `json:"ori_question,omitempty"`
	}{Query: query, Count: count, Question: question})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return web.SearchResult{}, &web.Error{Code: "WEB_PROVIDER_MISCONFIGURED", Message: "invalid DeepSeek Search endpoint", Cause: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "deepseek-harness/0.0.1")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	local := *client
	local.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := local.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return web.SearchResult{}, aborted(ctx.Err())
		}
		return web.SearchResult{}, c.failure(endpoint, "DeepSeek web request failed: TypeError: fetch failed", err)
	}
	defer res.Body.Close()
	if (res.StatusCode == 301 || res.StatusCode == 302 || res.StatusCode == 303 || res.StatusCode == 307 || res.StatusCode == 308) && res.Header.Get("Location") != "" {
		return web.SearchResult{}, c.failure(endpoint, "DeepSeek web request failed: TypeError: fetch failed", nil)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, (4<<20)+1))
	if ctx.Err() != nil {
		return web.SearchResult{}, aborted(ctx.Err())
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		message := fmt.Sprintf("DeepSeek API error (HTTP %d)", res.StatusCode)
		if detail := responseDetail(data); detail != "" {
			message += ": " + detail
		}
		return web.SearchResult{}, c.failure(endpoint, message, nil)
	}
	if err != nil {
		return web.SearchResult{}, c.failure(endpoint, "DeepSeek returned an unprocessable response body", err)
	}
	if len(data) > 4<<20 {
		return web.SearchResult{}, c.failure(endpoint, "DeepSeek search response exceeds 4 MiB", nil)
	}
	out, err := decodeSearchResult(data)
	if err != nil {
		return web.SearchResult{}, c.failure(endpoint, err.Error(), err)
	}
	return out, nil
}

func decodeSearchResult(data []byte) (web.SearchResult, error) {
	var response struct {
		Results []struct {
			URL     string          `json:"url"`
			Title   *string         `json:"title"`
			Snippet *string         `json:"snippet"`
			Summary json.RawMessage `json:"summary"`
			Date    json.RawMessage `json:"date"`
		} `json:"results"`
	}
	invalid := errors.New("DeepSeek returned an invalid response body")
	if json.Unmarshal(data, &response) != nil || response.Results == nil {
		return web.SearchResult{}, invalid
	}
	result := web.SearchResult{Sources: make([]web.SearchSource, 0, len(response.Results))}
	for _, row := range response.Results {
		if row.URL == "" || row.Title == nil || row.Snippet == nil || len(row.Date) == 0 {
			return web.SearchResult{}, invalid
		}
		var summary string
		if len(row.Summary) > 0 && (bytes.Equal(row.Summary, []byte("null")) || json.Unmarshal(row.Summary, &summary) != nil) {
			return web.SearchResult{}, invalid
		}
		var date string
		if err := json.Unmarshal(row.Date, &date); err != nil {
			return web.SearchResult{}, invalid
		}
		result.Sources = append(result.Sources, web.SearchSource{
			URL: row.URL, Title: nonempty(*row.Title), Snippet: nonempty(*row.Snippet),
			Summary: nonempty(summary), PublishedAt: nonempty(date),
		})
	}
	return result, nil
}

func nonempty(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}

func responseDetail(data []byte) string {
	var detail struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(data, &detail) != nil {
		return ""
	}
	var text string
	if len(detail.Error) > 0 && detail.Error[0] == '"' && json.Unmarshal(detail.Error, &text) == nil {
		return text
	}
	if len(detail.Error) > 0 && detail.Error[0] == '{' {
		var nested struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(detail.Error, &nested)
		return nested.Message
	}
	return detail.Message
}

func (c Client) failure(endpoint, message string, cause error) error {
	if c.APIKey != "" {
		message = strings.ReplaceAll(message, c.APIKey, "[redacted]")
	}
	message += "\n\nThe web request used endpoint " + strconv.Quote(endpoint) + ". Search uses the configured DeepSeek search API key. Only the user should choose or change the endpoint."
	return &web.Error{Code: "WEB_PROVIDER_ERROR", Message: message, Cause: cause}
}

func aborted(err error) error {
	return &web.Error{Code: "WEB_ABORTED", Message: "DeepSeek web request aborted", Cause: err}
}
