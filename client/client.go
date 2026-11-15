// Package client is a small HTTP client for personal Host session operations.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type HTTPError struct {
	Status int
	Code   string
}

func (e *HTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("harness HTTP %d: %s", e.Status, e.Code)
	}
	return fmt.Sprintf("harness HTTP %d", e.Status)
}

type Client struct {
	BaseURL string
	UserID  string
	HTTP    *http.Client
}

func (c Client) Do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.UserID != "" {
		request.Header.Set("X-User-ID", c.UserID)
	}
	transport := c.HTTP
	if transport == nil {
		transport = http.DefaultClient
	}
	res, err := transport.Do(request)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		var payload struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 8<<10)).Decode(&payload)
		return nil, &HTTPError{Status: res.StatusCode, Code: payload.Error}
	}
	return res, nil
}
func (c Client) CreateSession(ctx context.Context) (string, error) {
	res, err := c.Do(ctx, "POST", "/v1/sessions", nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		SessionID string `json:"sessionId"`
	}
	err = json.NewDecoder(res.Body).Decode(&out)
	return out.SessionID, err
}
func (c Client) Send(ctx context.Context, sessionID, text string) (string, error) {
	res, err := c.Do(ctx, "POST", "/v1/sessions/"+url.PathEscape(sessionID)+"/messages", map[string]string{"content": text})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		MessageID string `json:"messageId"`
	}
	err = json.NewDecoder(res.Body).Decode(&out)
	return out.MessageID, err
}

// Stream returns a live SSE body. The caller closes it and owns reconnect policy.
func (c Client) Stream(ctx context.Context, sessionID string, after *uint64) (io.ReadCloser, error) {
	path := "/v1/sessions/" + url.PathEscape(sessionID) + "/stream"
	if after != nil {
		path += fmt.Sprintf("?after=%d", *after)
	}
	res, err := c.Do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}
