package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client binds one endpoint, its API key, and a model. Calls use Responses by
// default; Chat Completions must be requested explicitly for incompatible models.
type Client struct {
	endpoint Endpoint
	model    string
	http     *http.Client
	// ServerAgents requests use a renewable no-progress deadline instead of
	// the default whole-request deadline. Streaming requests reset it on
	// provider events; non-streaming requests reset it on response I/O.
	nonStreamingIdleTimeout time.Duration
}

var errRequestIdle = fmt.Errorf("LLM request made no progress: %w", context.DeadlineExceeded)

// NewClientWithNonStreamingIdleTimeout gives ServerAgents model requests a
// renewable deadline. Response I/O or parsed streaming events reset the timer;
// caller cancellation remains distinct from an idle timeout.
func NewClientWithNonStreamingIdleTimeout(endpoint Endpoint, model string, idleTimeout time.Duration) (*Client, error) {
	return NewClientWithHTTPAndIdleTimeout(endpoint, model, &http.Client{}, idleTimeout)
}

// NewClientWithHTTPAndIdleTimeout keeps request encoding and response parsing
// in the caller while allowing a caller-owned HTTP transport.
func NewClientWithHTTPAndIdleTimeout(endpoint Endpoint, model string, httpClient *http.Client, idleTimeout time.Duration) (*Client, error) {
	if idleTimeout <= 0 {
		return nil, fmt.Errorf("LLM request idle timeout must be positive")
	}
	client, err := NewClient(endpoint, model, httpClient)
	if err != nil {
		return nil, err
	}
	client.nonStreamingIdleTimeout = idleTimeout
	return client, nil
}

func NewClient(endpoint Endpoint, model string, httpClient *http.Client) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(endpoint.BaseURL), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid LLM endpoint base URL")
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("LLM model is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Client{endpoint: Endpoint{BaseURL: base, APIKey: strings.TrimSpace(endpoint.APIKey)}, model: strings.TrimSpace(model), http: httpClient}, nil
}

func (c *Client) Generate(ctx context.Context, request Request) (*Response, error) {
	return c.GenerateWithProtocol(ctx, Responses, request)
}

func (c *Client) GenerateWithProtocol(ctx context.Context, protocol Protocol, request Request) (*Response, error) {
	path, body, err := c.requestBody(protocol, request, false)
	if err != nil {
		return nil, err
	}
	requestCtx := ctx
	var pulse func()
	if c.nonStreamingIdleTimeout > 0 {
		var cancel context.CancelCauseFunc
		requestCtx, cancel = context.WithCancelCause(ctx)
		timer := time.AfterFunc(c.nonStreamingIdleTimeout, func() { cancel(errRequestIdle) })
		defer func() {
			timer.Stop()
			cancel(nil)
		}()
		pulse = func() { timer.Reset(c.nonStreamingIdleTimeout) }
	}
	res, err := c.post(requestCtx, path, body, false)
	if err != nil {
		return nil, idleRequestError(requestCtx, errRequestIdle, err)
	}
	defer res.Body.Close()
	if pulse != nil {
		pulse()
	}
	reader := io.Reader(res.Body)
	if pulse != nil {
		reader = progressReader{Reader: reader, pulse: pulse}
	}
	data, err := io.ReadAll(io.LimitReader(reader, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read LLM response: %w", idleRequestError(requestCtx, errRequestIdle, err))
	}
	if c.nonStreamingIdleTimeout > 0 && errors.Is(context.Cause(requestCtx), errRequestIdle) {
		return nil, errRequestIdle
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, decodeAPIError(res.StatusCode, data)
	}
	switch protocol {
	case Responses:
		return decodeResponses(data)
	case ChatCompletions:
		return decodeChatCompletion(data)
	default:
		return nil, fmt.Errorf("unsupported LLM protocol %q", protocol)
	}
}

type progressReader struct {
	io.Reader
	pulse func()
}

func (r progressReader) Read(data []byte) (int, error) {
	n, err := r.Reader.Read(data)
	if n > 0 {
		r.pulse()
	}
	return n, err
}

func idleRequestError(ctx context.Context, idle, err error) error {
	if errors.Is(context.Cause(ctx), idle) {
		return idle
	}
	return err
}

func (c *Client) post(ctx context.Context, path string, body []byte, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.endpoint.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.endpoint.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if stream && c.nonStreamingIdleTimeout > 0 {
		// StreamWithProtocol owns a renewable idle watchdog. A whole-request
		// deadline would wrongly stop a long stream that keeps making progress.
		streamClient := *c.http
		streamClient.Timeout = 0
		return streamClient.Do(req)
	}
	return c.http.Do(req)
}

func (c *Client) requestBody(protocol Protocol, request Request, stream bool) (string, []byte, error) {
	if request.MaxOutputTokens < 0 {
		return "", nil, fmt.Errorf("negative maximum output tokens")
	}
	switch protocol {
	case Responses:
		input, err := encodeResponsesInput(request.Input)
		if err != nil {
			return "", nil, err
		}
		payload := map[string]any{"model": c.model, "input": input, "store": false, "stream": stream}
		if request.MaxOutputTokens > 0 {
			payload["max_output_tokens"] = request.MaxOutputTokens
		}
		effort := strings.TrimSpace(request.ReasoningEffort)
		if effort == "" {
			effort = "medium"
		}
		payload["reasoning"] = map[string]any{"effort": effort}
		if len(request.Tools) > 0 {
			payload["tools"] = encodeResponsesTools(request.Tools)
			payload["tool_choice"] = defaultToolChoice(request.ToolChoice)
		}
		body, err := json.Marshal(payload)
		return "/responses", body, err
	case ChatCompletions:
		messages, err := encodeChatMessages(request.Input)
		if err != nil {
			return "", nil, err
		}
		payload := map[string]any{"model": c.model, "messages": messages, "stream": stream}
		if request.MaxOutputTokens > 0 {
			payload["max_completion_tokens"] = request.MaxOutputTokens
		}
		if len(request.Tools) > 0 {
			payload["tools"] = encodeChatTools(request.Tools)
			payload["tool_choice"] = defaultToolChoice(request.ToolChoice)
		}
		body, err := json.Marshal(payload)
		return "/chat/completions", body, err
	default:
		return "", nil, fmt.Errorf("unsupported LLM protocol %q", protocol)
	}
}

func defaultToolChoice(value string) string {
	if choice := strings.TrimSpace(value); choice != "" {
		return choice
	}
	return "auto"
}

func decodeAPIError(status int, data []byte) error {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &payload)
	message := payload.Error.Message
	if message == "" {
		message = fmt.Sprintf("LLM API returned HTTP %d", status)
	}
	return &APIError{StatusCode: status, Code: payload.Error.Code, Message: message}
}
