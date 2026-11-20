package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"go-bot/internal/mcpclient"
	"go-bot/plugin"
)

func TestRPCOuterCapabilityErrorOwnsClassification(t *testing.T) {
	left, right := net.Pipe()
	host := newPeer(left, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, &mcpclient.Error{Code: "MCP_CONNECTION_FAILED", Message: "outer capability failure", Cause: &plugin.Error{Name: "PluginError", Code: "EXTENSION_CLOSED", Message: "inner failure"}}
	})
	node := newPeer(right, nil)
	t.Cleanup(host.close)
	t.Cleanup(node.close)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err := node.call(ctx, "mcp", nil, nil)
	var details *plugin.Error
	if !errors.As(err, &details) || details.Name != "McpError" || details.Code != "MCP_CONNECTION_FAILED" || details.Message != "outer capability failure" || details.Unclassified {
		t.Fatalf("an inner cause replaced the outer capability identity: %v", err)
	}
}

func TestRPCWrappedPluginErrorRetainsIdentity(t *testing.T) {
	left, right := net.Pipe()
	host := newPeer(left, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, fmt.Errorf("loading skill: %w", &plugin.Error{Name: "SkillError", Code: "SKILL_INVALID_DEFINITION", Message: "invalid definition"})
	})
	node := newPeer(right, nil)
	t.Cleanup(host.close)
	t.Cleanup(node.close)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err := node.call(ctx, "skill", nil, nil)
	var details *plugin.Error
	if !errors.As(err, &details) || details.Name != "SkillError" || details.Code != "SKILL_INVALID_DEFINITION" || details.Message != "loading skill: invalid definition" {
		t.Fatalf("wrapped plugin error changed across RPC: %v", err)
	}
}

func TestRPCNestedCallbackDoesNotDeadlock(t *testing.T) {
	left, right := net.Pipe()
	var host *peer
	host = newPeer(left, func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if method != "outer" {
			return nil, errors.New("unexpected request")
		}
		var answer string
		if err := host.call(ctx, "inner", map[string]int{"n": 42}, &answer); err != nil {
			return nil, err
		}
		return "nested:" + answer, nil
	})
	node := newPeer(right, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		if method != "inner" {
			return nil, errors.New("unexpected callback")
		}
		return "42", nil
	})
	t.Cleanup(host.close)
	t.Cleanup(node.close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var value string
	if err := node.call(ctx, "outer", nil, &value); err != nil || value != "nested:42" {
		t.Fatalf("callback: value=%q err=%v", value, err)
	}
}

func TestRPCCancellationAndOversizedResult(t *testing.T) {
	left, right := net.Pipe()
	started, settled := make(chan struct{}), make(chan struct{})
	host := newPeer(left, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "oversized" {
			return strings.Repeat("x", maxFrame), nil
		}
		close(started)
		<-ctx.Done()
		close(settled)
		return nil, ctx.Err()
	})
	node := newPeer(right, nil)
	t.Cleanup(host.close)
	t.Cleanup(node.close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- node.call(ctx, "wait", nil, nil) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request never started")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation: %v", err)
	}
	select {
	case <-settled:
	case <-time.After(3 * time.Second):
		t.Fatal("remote call was not cancelled")
	}
	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	var result any
	err := node.call(ctx, "oversized", nil, &result)
	var info *plugin.Error
	if !errors.As(err, &info) || info.Code != "INVALID_RESULT" {
		t.Fatalf("oversized response hung or was accepted: %v", err)
	}
}

func TestRPCDisconnectSettlesCallAndCancelsHandler(t *testing.T) {
	left, right := net.Pipe()
	started, settled := make(chan struct{}), make(chan struct{})
	host := newPeer(left, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		close(settled)
		return nil, ctx.Err()
	})
	node := newPeer(right, nil)
	t.Cleanup(host.close)
	t.Cleanup(node.close)
	done := make(chan error, 1)
	go func() { done <- node.call(context.Background(), "effect", nil, nil) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request never started")
	}
	node.close()
	select {
	case err := <-done:
		if !errors.Is(err, errDisconnected) {
			t.Fatalf("disconnect: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending call leaked")
	}
	select {
	case <-settled:
	case <-time.After(3 * time.Second):
		t.Fatal("handler context leaked")
	}
}
