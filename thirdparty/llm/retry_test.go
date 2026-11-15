package llm

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

type retryGenerator struct {
	calls int
	run   func(int, Request) (*Response, error)
}

func (g *retryGenerator) Generate(_ context.Context, request Request) (*Response, error) {
	g.calls++
	return g.run(g.calls, request)
}

func noRetryWait(_ context.Context, _ time.Duration) error { return nil }
func zeroJitter(_ int64) int64                             { return 0 }

func TestGenerateWithRetryRetriesTransientProviderAndTransportFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"rate quota", &APIError{StatusCode: 429, Code: "Throttling.RateQuota", Message: "Requests rate limit exceeded"}},
		{"allocation quota", &APIError{StatusCode: 429, Code: "Throttling.AllocationQuota", Message: "Allocated quota exceeded"}},
		{"burst rate", &APIError{StatusCode: 429, Code: "Throttling.BurstRate"}},
		{"server error", &APIError{StatusCode: 500, Code: "SystemError"}},
		{"model unavailable", &APIError{StatusCode: 503, Code: "ModelUnavailable"}},
		{"gateway timeout", &APIError{StatusCode: 504, Code: "GatewayTimeout"}},
		{"proxy server error", &APIError{StatusCode: 520}},
		{"unexpected EOF", io.ErrUnexpectedEOF},
		{"EOF", io.EOF},
		{"connection reset", errors.New("read response: connection reset by peer")},
		{"timeout", context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := Request{ReasoningEffort: "medium", MaxOutputTokens: 12}
			wanted := &Response{Status: "completed"}
			generator := &retryGenerator{run: func(call int, actual Request) (*Response, error) {
				if !reflect.DeepEqual(actual, request) {
					t.Fatalf("retry changed request: %#v", actual)
				}
				if call == 1 {
					return nil, tc.err
				}
				return wanted, nil
			}}
			got, err := generateWithRetry(context.Background(), generator, request,
				GenerateRetryPolicy{MaxRetries: 2}, noRetryWait, zeroJitter)
			if err != nil || got != wanted || generator.calls != 2 {
				t.Fatalf("response=%#v error=%v calls=%d", got, err, generator.calls)
			}
		})
	}
}

func TestGenerateWithRetryStopsAfterTwoBackoffRetries(t *testing.T) {
	failure := &APIError{StatusCode: 429, Code: "Throttling.RateQuota"}
	generator := &retryGenerator{run: func(_ int, _ Request) (*Response, error) { return nil, failure }}
	var delays []time.Duration
	var retryNumbers []int
	_, err := generateWithRetry(context.Background(), generator, Request{}, GenerateRetryPolicy{
		MaxRetries: 2, BaseDelay: time.Second, MaxDelay: 2 * time.Second,
		OnRetry: func(number int, delay time.Duration, _ error) { retryNumbers = append(retryNumbers, number) },
	}, func(_ context.Context, delay time.Duration) error { delays = append(delays, delay); return nil }, zeroJitter)
	if !errors.Is(err, failure) || generator.calls != 3 ||
		!reflect.DeepEqual(delays, []time.Duration{500 * time.Millisecond, time.Second}) ||
		!reflect.DeepEqual(retryNumbers, []int{1, 2}) {
		t.Fatalf("error=%v calls=%d delays=%v retry numbers=%v", err, generator.calls, delays, retryNumbers)
	}
}

func TestGenerateWithRetryDoesNotReplayPermanentFailuresOrObservedOutput(t *testing.T) {
	cases := []struct {
		name     string
		response *Response
		err      error
	}{
		{"invalid request", nil, &APIError{StatusCode: 400, Code: "InvalidParameter"}},
		{"authentication", nil, &APIError{StatusCode: 401, Code: "InvalidApiKey"}},
		{"budget exhausted", nil, &APIError{StatusCode: 429, Code: "BudgetLimitExceeded"}},
		{"unpaid account", nil, &APIError{StatusCode: 429, Code: "PrepaidBillOverdue"}},
		{"budget without code", nil, &APIError{StatusCode: 429, Message: "The budget configured in Budget Management has been exhausted"}},
		{"free quota exhausted", nil, &APIError{StatusCode: 429, Message: "Free allocated quota exceeded"}},
		{"invalid parameter with HTTP 500", nil, &APIError{StatusCode: 500, Code: "InternalError.Algo.InvalidParameter"}},
		{"application configuration failure", nil, &APIError{StatusCode: 500, Code: "AppProcessFailed"}},
		{"context length", nil, &APIError{StatusCode: 400, Code: "context_length_exceeded"}},
		{"DNS not found", nil, &net.DNSError{Err: "no such host", IsNotFound: true}},
		{"invalid TLS certificate", nil, x509.UnknownAuthorityError{Cert: &x509.Certificate{}}},
		{"unknown", nil, errors.New("unexpected local failure")},
		{"output observed", &Response{Output: []Item{{Type: "message"}}}, io.ErrUnexpectedEOF},
		{"typed output observed", nil, &ModelError{Code: ModelErrorUpstreamNetwork, OutputObserved: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			generator := &retryGenerator{run: func(_ int, _ Request) (*Response, error) { return tc.response, tc.err }}
			response, err := generateWithRetry(context.Background(), generator, Request{},
				GenerateRetryPolicy{MaxRetries: 2}, noRetryWait, zeroJitter)
			if response != tc.response || !errors.Is(err, tc.err) || generator.calls != 1 {
				t.Fatalf("response=%#v error=%v calls=%d", response, err, generator.calls)
			}
		})
	}
}

func TestGenerateWithRetryRecoversFromTruncatedHTTPBody(t *testing.T) {
	var calls int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"status":"completed"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"completed","output":[]}`))
	}))
	defer provider.Close()
	client, err := NewClient(Endpoint{BaseURL: provider.URL, APIKey: "test"}, "test-model", provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := GenerateWithRetry(context.Background(), client, Request{}, GenerateRetryPolicy{
		MaxRetries: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond,
	})
	if err != nil || response == nil || response.Status != "completed" || calls != 2 {
		t.Fatalf("response=%#v error=%v calls=%d", response, err, calls)
	}
}

func TestGenerateWithRetryStopsWhenCallerCancelsDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	generator := &retryGenerator{run: func(_ int, _ Request) (*Response, error) {
		return nil, &APIError{StatusCode: 503, Code: "ModelUnavailable"}
	}}
	response, err := generateWithRetry(ctx, generator, Request{}, GenerateRetryPolicy{MaxRetries: 2},
		func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}, zeroJitter)
	if response != nil || !errors.Is(err, context.Canceled) || generator.calls != 1 {
		t.Fatalf("response=%#v error=%v calls=%d", response, err, generator.calls)
	}
}
