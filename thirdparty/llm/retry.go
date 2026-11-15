package llm

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"math/rand"
	"net"
	"strings"
	"time"
)

// Generator is the non-streaming part of a model client.
type Generator interface {
	Generate(context.Context, Request) (*Response, error)
}

// GenerateRetryPolicy applies only where GenerateWithRetry is explicitly used.
type GenerateRetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	OnRetry    func(retryNumber int, delay time.Duration, err error)
}

// GenerateWithRetry repeats the identical non-streaming request for transient
// failures. A response that already contains model output or usage is never
// replayed, because the upstream may already have completed and billed it.
func GenerateWithRetry(ctx context.Context, client Generator, request Request, policy GenerateRetryPolicy) (*Response, error) {
	return generateWithRetry(ctx, client, request, policy, waitForRetry, rand.Int63n)
}

func generateWithRetry(ctx context.Context, client Generator, request Request, policy GenerateRetryPolicy,
	wait func(context.Context, time.Duration) error, randomInt63n func(int64) int64,
) (*Response, error) {
	if policy.MaxRetries < 0 {
		policy.MaxRetries = 0
	}
	if policy.BaseDelay <= 0 {
		policy.BaseDelay = time.Second
	}
	if policy.MaxDelay < policy.BaseDelay {
		policy.MaxDelay = policy.BaseDelay
	}
	for retryNumber := 0; ; retryNumber++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response, err := client.Generate(ctx, request)
		if err == nil || retryNumber >= policy.MaxRetries || !RetryableGenerateError(ctx, response, err) {
			return response, err
		}
		delay := generateRetryDelay(policy, retryNumber+1, randomInt63n)
		if policy.OnRetry != nil {
			policy.OnRetry(retryNumber+1, delay, err)
		}
		if waitErr := wait(ctx, delay); waitErr != nil {
			return nil, waitErr
		}
	}
}

func generateRetryDelay(policy GenerateRetryPolicy, retryNumber int, randomInt63n func(int64) int64) time.Duration {
	delay := policy.BaseDelay
	for attempt := 1; attempt < retryNumber && delay < policy.MaxDelay; attempt++ {
		if delay > policy.MaxDelay/2 {
			delay = policy.MaxDelay
		} else {
			delay *= 2
		}
	}
	if delay > policy.MaxDelay {
		delay = policy.MaxDelay
	}
	// Equal jitter spreads simultaneous retries without allowing an immediate retry.
	half := delay / 2
	return half + time.Duration(randomInt63n(int64(delay-half)+1))
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RetryableGenerateError applies the same provider-code policy used by the
// non-streaming retry driver; callers may also use it in their error response.
func RetryableGenerateError(ctx context.Context, response *Response, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || response != nil && (len(response.Output) > 0 || response.Usage != nil) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if permanentProviderError(apiErr.Code, apiErr.Message) {
			return false
		}
		if apiErr.StatusCode != 0 {
			return apiErr.StatusCode == 429 || apiErr.StatusCode == 408 || apiErr.StatusCode >= 500 && apiErr.StatusCode < 600
		}
	}
	modelErr := NormalizeModelError("", "", err)
	if modelErr.OutputObserved {
		return false
	}
	switch modelErr.Code {
	case ModelErrorUpstreamRateLimited, ModelErrorUpstreamTimeout,
		ModelErrorUpstreamOverloaded, ModelErrorUpstreamInternal,
		ModelErrorUpstreamNetwork, ModelErrorUpstreamStreamTruncated,
		ModelErrorUpstreamTemporaryUnavailable, ModelErrorResponseIncomplete:
		return !permanentTransportError(err)
	default:
		return false
	}
}

func permanentProviderError(code, message string) bool {
	value := strings.ToLower(strings.TrimSpace(code) + " " + strings.TrimSpace(message))
	// Bailian also uses HTTP 429 for exhausted budgets and unpaid accounts, and
	// some 5xx errors carry deterministic invalid-input codes.
	for _, marker := range []string{
		"commoditynotpurchased", "prepaidbilloverdue", "postpaidbilloverdue", "budgetlimitexceeded",
		"free allocated quota exceeded", "billing or quota exhausted", "insufficient balance", "insufficient_balance", "insufficientbalance",
		"budget management has been exhausted", "prepaid bill", "postpaid bill",
		"invalidparameter", "invalid_parameter", "invalid parameter", "invalid request", "invalid_request",
		"authentication", "unauthorized", "permission denied", "access denied", "accessdenied",
		"content policy", "contentpolicy", "data inspection", "datainspectionfailed", "model not found", "model_not_found",
		"appprocessfailed",
		"context length", "context_length", "maximum context", "token length exceed",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func permanentTransportError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return true
	}
	var unknownAuthority x509.UnknownAuthorityError
	var invalidCertificate x509.CertificateInvalidError
	var hostnameError x509.HostnameError
	if errors.As(err, &unknownAuthority) || errors.As(err, &invalidCertificate) || errors.As(err, &hostnameError) {
		return true
	}
	return errors.Is(err, io.ErrClosedPipe)
}
