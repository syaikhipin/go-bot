package authorization

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

type callbackBaseKey struct{}

// WithCallbackBase is called by the authenticated Controller-to-Host adapter.
// A business plugin cannot select another account or an arbitrary HTTP route.
func WithCallbackBase(ctx context.Context, base string) context.Context {
	return context.WithValue(ctx, callbackBaseKey{}, base)
}
func callbackBase(ctx context.Context) string {
	value, _ := ctx.Value(callbackBaseKey{}).(string)
	return value
}

type surfaceCallback struct {
	token, state, url string
	ready             chan struct{}
	query             map[string][]string
}

func (s *Surface) prepareCallback(ctx context.Context, a *surfaceAttempt, state string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if state == "" || len(state) > 1024 || strings.ContainsAny(state, "\x00\r\n") {
		return "", failure("INVALID_CALLBACK", "Provide the provider library's expected authorization state")
	}
	u, err := url.Parse(a.callbackBase)
	if err != nil || u == nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !strings.HasSuffix(u.Path, "/") || len(a.callbackBase) > 4096 {
		return "", failure("CALLBACK_UNAVAILABLE", "Browser authorization requires a verified Controller callback through HTTPS or localhost HTTP")
	}
	loopback := strings.EqualFold(u.Hostname(), "localhost") || net.ParseIP(u.Hostname()).IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", failure("CALLBACK_UNAVAILABLE", "Browser authorization requires HTTPS or localhost HTTP")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.ctx.Err() != nil || a.view.Status != "running" {
		return "", context.Canceled
	}
	if a.callback != nil {
		if a.callback.state != state {
			return "", failure("CALLBACK_CONFLICT", "This authorization attempt already has a callback")
		}
		return a.callback.url, nil
	}
	token := uuid.NewString()
	a.callback = &surfaceCallback{token: token, state: state, url: a.callbackBase + token, ready: make(chan struct{})}
	return a.callback.url, nil
}

func (s *Surface) waitCallback(ctx context.Context, a *surfaceAttempt) (map[string][]string, error) {
	s.mu.Lock()
	callback := a.callback
	s.mu.Unlock()
	if callback == nil {
		return nil, failure("CALLBACK_UNAVAILABLE", "Create an authorization callback before waiting for it")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-a.ctx.Done():
		return nil, a.ctx.Err()
	case <-callback.ready:
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.ctx.Err() != nil || a.callback != callback || a.view.Status != "running" {
		return nil, context.Canceled
	}
	query := make(map[string][]string, len(callback.query))
	for key, value := range callback.query {
		query[key] = append([]string(nil), value...)
	}
	return query, nil
}

// Receiving a callback is not proof of authorization. Only the business flow
// can validate its provider response, exchange tokens and commit a credential.
func (s *Surface) receiveCallback(token string, query map[string][]string) (any, error) {
	data, err := json.Marshal(query)
	if err != nil || len(data) > 24<<10 || len(query) > 64 || len(query["state"]) != 1 {
		return nil, failure("INVALID_CALLBACK", "Invalid authorization callback")
	}
	if _, err := uuid.Parse(token); err != nil {
		return nil, failure("INVALID_CALLBACK", "Invalid authorization callback")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.attempts {
		c := a.callback
		if c == nil || c.token != token {
			continue
		}
		if a.ctx.Err() != nil || a.view.Status != "running" || c.query != nil || subtle.ConstantTimeCompare([]byte(c.state), []byte(query["state"][0])) != 1 {
			return nil, failure("INVALID_CALLBACK", "Authorization callback expired, was already used, or has an invalid state")
		}
		c.query = make(map[string][]string, len(query))
		for key, value := range query {
			c.query[key] = append([]string(nil), value...)
		}
		close(c.ready)
		return map[string]bool{"received": true}, nil
	}
	return nil, failure("INVALID_CALLBACK", "Authorization callback is no longer available")
}

func (a *Session) Callback(ctx context.Context, state string) (string, error) {
	if err := a.Context.Err(); err != nil {
		return "", err
	}
	if a.interaction.Callback == nil {
		return "", failure("CALLBACK_UNAVAILABLE", "This authorization surface does not provide browser callbacks")
	}
	return a.interaction.Callback(ctx, state)
}
func (a *Session) WaitCallback(ctx context.Context) (map[string][]string, error) {
	if a.interaction.WaitCallback == nil {
		return nil, failure("CALLBACK_UNAVAILABLE", "This authorization surface does not provide browser callbacks")
	}
	call, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.Context, cancel)
	defer stop()
	defer cancel()
	return a.interaction.WaitCallback(call)
}
