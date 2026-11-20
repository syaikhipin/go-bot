package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

type StreamEvent struct {
	Type         string
	OutputIndex  int
	ContentIndex int
	Delta        string
	Item         *Item
	Response     *Response
}

type StreamSink func(StreamEvent) error

func (c *Client) Stream(ctx context.Context, request Request, sink StreamSink) (*Response, error) {
	return c.StreamWithProtocol(ctx, Responses, request, sink)
}

func (c *Client) StreamWithProtocol(ctx context.Context, protocol Protocol, request Request, sink StreamSink) (*Response, error) {
	path, body, err := c.requestBody(protocol, request, true)
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
	res, err := c.post(requestCtx, protocol, path, body, true)
	if err != nil {
		return nil, idleRequestError(requestCtx, errRequestIdle, err)
	}
	defer res.Body.Close()
	if pulse != nil {
		pulse()
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return nil, decodeAPIError(res.StatusCode, data)
	}
	if sink == nil {
		sink = func(StreamEvent) error { return nil }
	}
	if pulse != nil {
		caller := sink
		sink = func(event StreamEvent) error {
			pulse()
			return caller(event)
		}
	}
	var result *Response
	switch protocol {
	case AnthropicMessages:
		result, err = consumeAnthropicStream(res.Body, sink, pulse)
	case Responses:
		result, err = consumeResponsesStream(res.Body, sink, pulse)
	case ChatCompletions:
		result, err = consumeChatStream(res.Body, sink, pulse)
	default:
		return nil, fmt.Errorf("unsupported LLM protocol %q", protocol)
	}
	if err != nil {
		return result, idleRequestError(requestCtx, errRequestIdle, err)
	}
	return result, nil
}

func readSSE(body io.Reader, onData func([]byte) (bool, error), activity func()) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	data := make([]string, 0, 1)
	dispatch := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		joined := strings.Join(data, "\n")
		data = data[:0]
		if activity != nil {
			activity()
		}
		return onData([]byte(joined))
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			done, err := dispatch()
			if done || err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			if activity != nil {
				activity()
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read LLM stream: %w", err)
	}
	// An EOF without the blank line terminator is a partial frame, not an
	// event. In particular it must not settle a truncated [DONE] or response.
	return nil
}

func consumeResponsesStream(body io.Reader, sink StreamSink, activity func()) (*Response, error) {
	items := make(map[int]*Item)
	var final *Response
	var terminalErr error
	err := readSSE(body, func(data []byte) (bool, error) {
		if string(data) == "[DONE]" {
			return true, nil
		}
		var event struct {
			Type         string          `json:"type"`
			Code         string          `json:"code"`
			Message      string          `json:"message"`
			OutputIndex  int             `json:"output_index"`
			ContentIndex int             `json:"content_index"`
			ItemID       string          `json:"item_id"`
			Item         json.RawMessage `json:"item"`
			Delta        string          `json:"delta"`
			Arguments    string          `json:"arguments"`
			Response     json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			return false, fmt.Errorf("decode Responses stream event: %w", err)
		}
		if event.Type == "error" {
			return false, &APIError{Code: event.Code, Message: event.Message}
		}
		index := event.OutputIndex
		current := items[index]
		if current == nil && (event.Type == "response.output_item.added" || event.Type == "response.output_item.done" || event.Type == "response.output_text.delta" || event.Type == "response.reasoning_text.delta" || event.Type == "response.reasoning_summary_text.delta" || event.Type == "response.function_call_arguments.delta" || event.Type == "response.function_call_arguments.done") {
			current = &Item{Type: "message", Role: "assistant"}
			items[index] = current
		}
		var emitted StreamEvent
		emitted.Type, emitted.OutputIndex, emitted.ContentIndex = event.Type, index, event.ContentIndex
		switch event.Type {
		case "response.output_item.added", "response.output_item.done":
			decoded, err := decodeResponsesOutputItem(event.Item)
			if err != nil {
				return false, err
			}
			if decoded != nil {
				if current.ID != "" && decoded.ID == "" {
					decoded.ID = current.ID
				}
				if len(decoded.Content) == 0 && len(current.Content) > 0 {
					decoded.Content = current.Content
				}
				if decoded.Arguments == "" && current.Arguments != "" {
					decoded.Arguments = current.Arguments
				}
				items[index] = decoded
				current = decoded
			}
			emitted.Item = cloneItem(current)
		case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			if strings.Contains(event.Type, "reasoning") {
				current.Type = "reasoning"
			}
			if event.ItemID != "" {
				current.ID = event.ItemID
			}
			for len(current.Content) <= event.ContentIndex {
				current.Content = append(current.Content, ContentBlock{Type: "text"})
			}
			current.Content[event.ContentIndex].Text += event.Delta
			emitted.Delta = event.Delta
			emitted.Item = cloneItem(current)
		case "response.function_call_arguments.delta":
			current.Type = "tool_call"
			if event.ItemID != "" {
				current.ID = event.ItemID
			}
			current.Arguments += event.Delta
			emitted.Delta = event.Delta
			emitted.Item = cloneItem(current)
		case "response.function_call_arguments.done":
			current.Type = "tool_call"
			if event.Arguments != "" {
				current.Arguments = event.Arguments
			}
			emitted.Item = cloneItem(current)
		case "response.completed", "response.incomplete", "response.failed", "response.cancelled":
			if len(event.Response) == 0 {
				return false, fmt.Errorf("Responses terminal event omitted response")
			}
			decoded, err := decodeResponses(event.Response)
			if decoded != nil && len(decoded.Output) == 0 {
				decoded.Output = orderedItems(items)
			}
			final = decoded
			emitted.Response = final
			terminalErr = err
		default:
			return false, nil
		}
		if err := sink(emitted); err != nil {
			return false, err
		}
		return final != nil, nil
	}, activity)
	partial := func() *Response {
		if final != nil {
			return final
		}
		if len(items) > 0 {
			return &Response{Output: orderedItems(items)}
		}
		return nil
	}
	if err != nil {
		return partial(), err
	}
	if terminalErr != nil {
		return partial(), terminalErr
	}
	if final == nil {
		return partial(), fmt.Errorf("Responses stream ended without completed response")
	}
	return final, terminalErr
}

func decodeResponsesOutputItem(raw json.RawMessage) (*Item, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(struct {
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
	}{Status: "completed", Output: []json.RawMessage{raw}})
	if err != nil {
		return nil, err
	}
	decoded, err := decodeResponses(payload)
	if err != nil {
		return nil, err
	}
	return &decoded.Output[0], nil
}

func orderedItems(items map[int]*Item) []Item {
	keys := make([]int, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	out := make([]Item, 0, len(keys))
	for _, key := range keys {
		out = append(out, *items[key])
	}
	return out
}

func cloneItem(item *Item) *Item {
	copy := *item
	copy.Content = append([]ContentBlock(nil), item.Content...)
	return &copy
}
