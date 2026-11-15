//go:build linux

package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go-harness/harness/agent"
)

func TestExecutorCancelGivesProcessGroupTermGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	workdir := t.TempDir()
	finished := make(chan struct {
		output string
		err    error
	}, 1)
	go func() {
		result, err := (Executor{verified: true}).RunStreaming(ctx,
			"trap 'printf term-handled; exit 0' TERM; printf ready; while :; do sleep 0.1; done",
			workdir, 4096, func(stream string, chunk []byte) {
				if stream == "stdout" && strings.Contains(string(chunk), "ready") {
					select {
					case <-ready:
					default:
						close(ready)
					}
				}
			})
		finished <- struct {
			output string
			err    error
		}{result.Stdout, err}
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("shell did not start")
	}
	cancel()
	select {
	case outcome := <-finished:
		if !errors.Is(outcome.err, context.Canceled) || !strings.Contains(outcome.output, "term-handled") {
			t.Fatalf("SIGTERM handler did not run before cancellation settled: %#v", outcome)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("cancelled shell did not settle")
	}
}

func TestExecutorCancelEscalatesTermResistantGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	finished := make(chan error, 1)
	workdir := t.TempDir()
	go func() {
		_, err := (Executor{verified: true}).RunStreaming(ctx,
			"trap '' TERM; printf ready; while :; do sleep 0.1; done",
			workdir, 4096, func(stream string, chunk []byte) {
				if stream == "stdout" && strings.Contains(string(chunk), "ready") {
					select {
					case <-ready:
					default:
						close(ready)
					}
				}
			})
		finished <- err
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("TERM-resistant shell did not start")
	}
	start := time.Now()
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) || errors.Is(err, agent.ErrToolOutcomeUnknown) {
			t.Fatalf("cancelled shell error = %v", err)
		}
		if time.Since(start) < shellTerminationGrace {
			t.Fatal("TERM-resistant shell was killed before its grace period")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("TERM-resistant process group survived escalation")
	}
}

func TestExecutorCancelEscalatesChildAfterParentExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	workdir := t.TempDir()
	finished := make(chan struct {
		output string
		err    error
	}, 1)
	go func() {
		result, err := (Executor{verified: true}).RunStreaming(ctx,
			"sh -c 'trap \"\" TERM; printf child-ready; while :; do sleep 0.1; done' & trap 'printf parent-term; exit 0' TERM; while :; do sleep 0.1; done",
			workdir, 4096, func(stream string, chunk []byte) {
				if stream == "stdout" && strings.Contains(string(chunk), "child-ready") {
					select {
					case <-ready:
					default:
						close(ready)
					}
				}
			})
		finished <- struct {
			output string
			err    error
		}{result.Stdout, err}
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("child shell did not start")
	}
	start := time.Now()
	cancel()
	select {
	case outcome := <-finished:
		if !errors.Is(outcome.err, context.Canceled) || errors.Is(outcome.err, agent.ErrToolOutcomeUnknown) {
			t.Fatalf("cancelled child process group error = %v", outcome.err)
		}
		if !strings.Contains(outcome.output, "parent-term") {
			t.Fatalf("parent did not exit on SIGTERM: %q", outcome.output)
		}
		if time.Since(start) < shellTerminationGrace {
			t.Fatal("TERM-resistant child exited before its grace period")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("child survived parent exit and process group escalation")
	}
}
