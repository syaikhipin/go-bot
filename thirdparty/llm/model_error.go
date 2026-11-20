package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type ModelErrorCode string

const (
	ModelErrorUpstreamNetwork              ModelErrorCode = "AI_UPSTREAM_NETWORK"
	ModelErrorUpstreamStreamTruncated      ModelErrorCode = "AI_UPSTREAM_STREAM_TRUNCATED"
	ModelErrorUpstreamRateLimited          ModelErrorCode = "AI_UPSTREAM_RATE_LIMITED"
	ModelErrorUpstreamTimeout              ModelErrorCode = "AI_UPSTREAM_TIMEOUT"
	ModelErrorUpstreamOverloaded           ModelErrorCode = "AI_UPSTREAM_OVERLOADED"
	ModelErrorUpstreamInternal             ModelErrorCode = "AI_UPSTREAM_INTERNAL"
	ModelErrorResponseIncomplete           ModelErrorCode = "AI_RESPONSE_INCOMPLETE"
	ModelErrorUpstreamTemporaryUnavailable ModelErrorCode = "AI_UPSTREAM_TEMPORARY_UNAVAILABLE"
	ModelErrorInvalidRequest               ModelErrorCode = "AI_INVALID_REQUEST"
	ModelErrorAuthenticationFailed         ModelErrorCode = "AI_AUTHENTICATION_FAILED"
	ModelErrorPermissionDenied             ModelErrorCode = "AI_PERMISSION_DENIED"
	ModelErrorModelNotFound                ModelErrorCode = "AI_MODEL_NOT_FOUND"
	ModelErrorBillingOrQuotaExhausted      ModelErrorCode = "AI_BILLING_OR_QUOTA_EXHAUSTED"
	ModelErrorContentPolicyRejected        ModelErrorCode = "AI_CONTENT_POLICY_REJECTED"
	ModelErrorContextLimitExceeded         ModelErrorCode = "AI_CONTEXT_LIMIT_EXCEEDED"
	ModelErrorCancelled                    ModelErrorCode = "AI_CANCELLED"
	ModelErrorUnknown                      ModelErrorCode = "AI_UNKNOWN"
)

type ModelErrorPhase string

const (
	ModelErrorPhaseRequest ModelErrorPhase = "request"
	ModelErrorPhaseStream  ModelErrorPhase = "stream"
)

// ModelError keeps transport failures typed for callers that retry model work.
// Cause preserves the upstream diagnostic.
type ModelError struct {
	Code           ModelErrorCode
	Endpoint       string
	Model          string
	UpstreamCode   string
	HTTPStatus     int
	RequestID      string
	Phase          ModelErrorPhase
	OutputObserved bool
	RetryAfter     time.Duration
	Cause          error
}

func (e *ModelError) Error() string {
	if e == nil {
		return "model request failed"
	}
	detail := "model request failed"
	if e.Cause != nil {
		detail = e.Cause.Error()
	}
	return fmt.Sprintf("%s: %s", e.Code, detail)
}

func (e *ModelError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *ModelError) MaxRetries() int {
	if e == nil {
		return 0
	}
	switch e.Code {
	case ModelErrorUpstreamTimeout,
		ModelErrorUpstreamOverloaded,
		ModelErrorUpstreamInternal,
		ModelErrorUpstreamRateLimited:
		return 2
	case ModelErrorUpstreamNetwork,
		ModelErrorUpstreamStreamTruncated,
		ModelErrorResponseIncomplete,
		ModelErrorUpstreamTemporaryUnavailable:
		return 1
	default:
		return 0
	}
}

func (e *ModelError) Retryable() bool { return e != nil && e.MaxRetries() > 0 }

// NewModelAPIError classifies a structured upstream API failure.
func NewModelAPIError(endpoint, model, upstreamCode, message string, status int) *ModelError {
	detail := strings.TrimSpace(message)
	if detail == "" {
		detail = http.StatusText(status)
	}
	if detail == "" {
		detail = "upstream model request failed"
	}
	cause := fmt.Errorf("endpoint=%s status=%d code=%s: %s", endpoint, status, strings.TrimSpace(upstreamCode), detail)
	result := classifyModelError(endpoint, model, status, upstreamCode, detail, cause)
	result.UpstreamCode = strings.TrimSpace(upstreamCode)
	result.HTTPStatus = status
	return result
}

// NormalizeModelError classifies HTTP, network, cancellation, and stream errors.
func NormalizeModelError(endpoint, model string, err error) *ModelError {
	if err == nil {
		return nil
	}
	var existing *ModelError
	if errors.As(err, &existing) {
		copy := *existing
		if copy.Endpoint == "" {
			copy.Endpoint = endpoint
		}
		if copy.Model == "" {
			copy.Model = model
		}
		return &copy
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		result := NewModelAPIError(endpoint, model, apiErr.Code, apiErr.Message, apiErr.StatusCode)
		result.Cause = err
		return result
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return modelError(ModelErrorUpstreamTimeout, endpoint, model, err)
	}
	if errors.Is(err, context.Canceled) {
		return modelError(ModelErrorCancelled, endpoint, model, err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		result := modelError(ModelErrorUpstreamStreamTruncated, endpoint, model, err)
		result.Phase = ModelErrorPhaseStream
		return result
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return modelError(ModelErrorUpstreamTimeout, endpoint, model, err)
		}
		return modelError(ModelErrorUpstreamNetwork, endpoint, model, err)
	}
	return classifyModelError(endpoint, model, 0, "", err.Error(), err)
}

func modelError(code ModelErrorCode, endpoint, model string, cause error) *ModelError {
	return &ModelError{Code: code, Endpoint: endpoint, Model: model, Phase: ModelErrorPhaseRequest, Cause: cause}
}

func classifyModelError(endpoint, model string, status int, upstreamCode, message string, cause error) *ModelError {
	text := strings.ToLower(strings.Join([]string{upstreamCode, message}, " "))
	code := ModelErrorUnknown
	switch {
	case containsAny(text, "context canceled", "request cancelled", "request canceled", "cancelled by user", "canceled by user"):
		code = ModelErrorCancelled
	case containsAny(text, "timeout", "timed out", "deadline exceeded", "requesttimeout", "internalerror.timeout"):
		code = ModelErrorUpstreamTimeout
	case containsAny(text, "unexpected eof", "unexpected end of json", "incomplete json"):
		code = ModelErrorUpstreamStreamTruncated
	case containsAny(text, "invalid api key", "invalid_api_key", "authentication", "unauthorized", "incorrect api key"):
		code = ModelErrorAuthenticationFailed
	case containsAny(
		text,
		"arrearage",
		"insufficient balance",
		"insufficient_balance",
		"free tier has been exhausted",
		"quota exhausted",
		"余额不足",
		"免费额度用尽",
	):
		code = ModelErrorBillingOrQuotaExhausted
	case containsAny(text, "permission denied", "accessdenied", "access denied", "forbidden", "model.accessdenied"):
		code = ModelErrorPermissionDenied
	case containsAny(text, "datainspectionfailed", "data_inspection_failed", "sensitive content", "inappropriate content", "content policy", "content_policy", "refusal"):
		code = ModelErrorContentPolicyRejected
	case containsAny(text, "context length", "context_length", "ai_context_limit_exceeded", "maximum context", "token length exceed", "too many tokens", "prompt is too long", "model_context_window_exceeded"):
		code = ModelErrorContextLimitExceeded
	case containsAny(text, "model not found", "model_not_found", "model does not exist", "model not exist", "endpoint not found"):
		code = ModelErrorModelNotFound
	case status == http.StatusTooManyRequests || containsAny(
		text,
		"throttling",
		"rate limit",
		"rate_limit",
		"ratelimit",
		"rate increased too quickly",
		"too many requests",
		"too many concurrent requests",
		"limit_requests",
		"limit_burst_rate",
		"qps",
		"rpm",
		"tpm",
		"请求过多",
		"限流",
	):
		code = ModelErrorUpstreamRateLimited
	case status == http.StatusBadGateway || status == http.StatusServiceUnavailable || containsAny(text, "overload", "overloaded", "service unavailable", "temporarily unavailable", "server busy", "服务繁忙", "系统繁忙"):
		code = ModelErrorUpstreamOverloaded
	case status == http.StatusGatewayTimeout || status == http.StatusRequestTimeout:
		code = ModelErrorUpstreamTimeout
	case status >= http.StatusInternalServerError || containsAny(text, "server_error", "internalerror", "internal error", "systemerror", "modelservicefailed", "model service failed", "inference engine"):
		code = ModelErrorUpstreamInternal
	case containsAny(text, "connection reset", "broken pipe", "connection refused", "no such host", "network is unreachable", "tls handshake", "connection error", "dial tcp"):
		code = ModelErrorUpstreamNetwork
	case status == http.StatusBadRequest || containsAny(text, "invalid parameter", "invalid_parameter", "invalid request", "invalid_request", "bad request"):
		code = ModelErrorInvalidRequest
	case status == http.StatusUnauthorized:
		code = ModelErrorAuthenticationFailed
	case status == http.StatusForbidden:
		code = ModelErrorPermissionDenied
	case status == http.StatusNotFound:
		code = ModelErrorModelNotFound
	}
	return modelError(code, endpoint, model, cause)
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
