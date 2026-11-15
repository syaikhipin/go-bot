package llm

import (
	"context"
	"errors"
	"testing"
)

func TestNormalizeModelErrorPreservesEndpointAndAPIClassification(t *testing.T) {
	upstream := &APIError{StatusCode: 429, Code: "rate_limit_exceeded", Message: "too many requests"}
	wrapped := errors.Join(errors.New("model request failed"), upstream)
	got := NormalizeModelError("https://models.example.test", "fast-model", wrapped)
	if got.Code != ModelErrorUpstreamRateLimited || got.Endpoint != "https://models.example.test" || got.Model != "fast-model" || got.UpstreamCode != "rate_limit_exceeded" || !errors.Is(got, upstream) || !got.Retryable() {
		t.Fatalf("unexpected model error: %+v", got)
	}
}

func TestNormalizeModelErrorClassifiesCancellationAndContextLimit(t *testing.T) {
	if got := NormalizeModelError("endpoint", "model", context.Canceled); got.Code != ModelErrorCancelled || got.Retryable() {
		t.Fatalf("unexpected cancellation: %+v", got)
	}
	if got := NormalizeModelError("endpoint", "model", &APIError{StatusCode: 400, Message: "maximum context length exceeded"}); got.Code != ModelErrorContextLimitExceeded || got.Retryable() {
		t.Fatalf("unexpected context limit: %+v", got)
	}
}

func TestNormalizeModelErrorClassifiesStreamServerErrorWithoutHTTPStatus(t *testing.T) {
	got := NormalizeModelError("endpoint", "model", &APIError{Code: "server_error", Message: "The model failed to generate a response."})
	if got.Code != ModelErrorUpstreamInternal || got.UpstreamCode != "server_error" || !got.Retryable() {
		t.Fatalf("stream server error classification = %+v", got)
	}
}
