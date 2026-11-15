package workflow

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmbeddedScriptTopLevelAwaitAndParallelFanout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan string, 2)
	release := make(chan struct{})
	type outcome struct {
		value any
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := Execute(ctx, `
const reviews = await parallel([
  () => agent(args.first),
  () => agent(args.second),
]);
return { reviews };
`, map[string]any{"first": "alpha", "second": "beta"}, func(ctx context.Context, prompt string) (any, error) {
			started <- prompt
			select {
			case <-release:
				return prompt + " reviewed", nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		done <- outcome{value, err}
	}()
	first, second := "", ""
	select {
	case first = <-started:
	case <-ctx.Done():
		t.Fatal("first child never started")
	}
	select {
	case second = <-started:
	case <-ctx.Done():
		t.Fatal("parallel did not start the second child before the first settled")
	}
	if first == second || (first != "alpha" && first != "beta") || (second != "alpha" && second != "beta") {
		t.Fatalf("unexpected child prompts %q, %q", first, second)
	}
	close(release)
	select {
	case result := <-done:
		if result.err != nil || !reflect.DeepEqual(result.value, map[string]any{"reviews": []any{"alpha reviewed", "beta reviewed"}}) {
			t.Fatalf("workflow result = %#v, %v", result.value, result.err)
		}
	case <-ctx.Done():
		t.Fatal("workflow did not settle")
	}
}

func TestEmbeddedScriptPipelineUsesPerItemStages(t *testing.T) {
	value, err := Execute(context.Background(), `
const values = await pipeline(args.items,
  async (_previous, item) => agent(item),
  async (previous) => previous + "!",
);
return values;
`, map[string]any{"items": []string{"a", "b"}}, func(_ context.Context, prompt string) (any, error) {
		return prompt + " done", nil
	})
	if err != nil || !reflect.DeepEqual(value, []any{"a done!", "b done!"}) {
		t.Fatalf("pipeline result = %#v, %v", value, err)
	}
}

func TestEmbeddedScriptRejectsInvalidSyntaxAndCancelsPendingWork(t *testing.T) {
	if _, err := Compile(`const = ;`); err == nil {
		t.Fatal("invalid JavaScript was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := Execute(ctx, `return await agent("wait");`, nil, func(ctx context.Context, _ string) (any, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("delegate did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled workflow did not stop")
	}
}

func TestEmbeddedScriptInterruptsCpuLoopAndHasNoNodeGlobals(t *testing.T) {
	value, err := Execute(context.Background(), `
return {
  require: typeof require,
  process: typeof process,
  fetch: typeof fetch,
  timer: typeof setTimeout,
};
`, nil, func(context.Context, string) (any, error) { return nil, nil })
	if err != nil || !reflect.DeepEqual(value, map[string]any{
		"require": "undefined", "process": "undefined", "fetch": "undefined", "timer": "undefined",
	}) {
		t.Fatalf("unexpected script globals: %#v, %v", value, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Execute(ctx, `while (true) {}`, nil, func(context.Context, string) (any, error) { return nil, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CPU-bound script was not interrupted: %v", err)
	}
}

func TestEmbeddedScriptBoundsInitialSynchronousSliceWithoutCallerDeadline(t *testing.T) {
	start := time.Now()
	_, err := ExecuteRun(context.Background(), `while (true) {}`, nil,
		Limits{SyncTimeout: 40 * time.Millisecond}, Observer{},
		func(context.Context, string, AgentOptions) (AgentResult, error) { return AgentResult{}, nil })
	if err == nil || !strings.Contains(err.Error(), "initial synchronous slice exceeded") {
		t.Fatalf("initial synchronous timeout = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("initial synchronous timeout returned too late: %s", time.Since(start))
	}
}

func TestWorkflowHooksOptionsProgressAndFailureClassification(t *testing.T) {
	var phases, logs []string
	var seen AgentOptions
	result, err := ExecuteRun(context.Background(), `
phase("review"); log("starting");
const items = await parallel([
  () => agent("good", {label:"one", model:"fast", provider:"primary", schema:{type:"object", properties:{ok:{type:"boolean"}}}}),
  () => agent("failed"),
  () => { throw new Error("ordinary stage error"); },
]);
return {items};
`, nil, Limits{MaxConcurrentAgents: 3}, Observer{
		Phase: func(value string) { phases = append(phases, value) },
		Log:   func(value string) { logs = append(logs, value) },
	}, func(_ context.Context, prompt string, options AgentOptions) (AgentResult, error) {
		if prompt == "good" {
			seen = options
			return AgentResult{Value: map[string]any{"ok": true}, Completed: true}, nil
		}
		return AgentResult{Completed: false}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AgentsStarted != 2 || !reflect.DeepEqual(result.Value, map[string]any{"items": []any{map[string]any{"ok": true}, nil, nil}}) {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(phases, []string{"review"}) || !reflect.DeepEqual(logs, []string{"starting"}) {
		t.Fatalf("progress = %#v, %#v", phases, logs)
	}
	if seen.Label != "one" || seen.Phase != "review" || seen.Model != "fast" || seen.Provider != "primary" || seen.Schema["type"] != "object" {
		t.Fatalf("options = %#v", seen)
	}
}

func TestWorkflowFatalHookAndProviderFailuresDoNotBecomeNull(t *testing.T) {
	delegate := func(context.Context, string, AgentOptions) (AgentResult, error) {
		return AgentResult{Completed: true, Value: "ok"}, nil
	}
	cases := []struct {
		script string
		limits Limits
		want   string
	}{
		{`return await parallel([() => agent("x", {unknown:true})]);`, Limits{}, "not supported"},
		{`return await pipeline([1], () => agent("x", {effort:"high"}));`, Limits{}, "not supported"},
		{`return await parallel([() => agent("x"), () => agent("y")]);`, Limits{MaxTotalAgents: 1}, "total agent cap"},
		{`return await parallel([() => 1, () => 2]);`, Limits{MaxItemsPerCall: 1}, "item cap"},
		{`return await pipeline([1]);`, Limits{}, "stage functions"},
		{`return await parallel([() => agent("x", {schema:{type:"string"}})]);`, Limits{}, "schema"},
		{`return {bad: undefined};`, Limits{}, "not plain JSON data"},
		{`return [1,,3];`, Limits{}, "not plain JSON data"},
	}
	for _, test := range cases {
		_, err := ExecuteRun(context.Background(), test.script, nil, test.limits, Observer{}, delegate)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("script %q: want %q, got %v", test.script, test.want, err)
		}
	}
	_, err := ExecuteRun(context.Background(), `return await parallel([() => agent("x")]);`, nil, Limits{}, Observer{}, func(context.Context, string, AgentOptions) (AgentResult, error) {
		return AgentResult{}, errors.New("child transport unavailable")
	})
	if err == nil || !strings.Contains(err.Error(), "child provider failed") {
		t.Fatalf("provider error was swallowed: %v", err)
	}
	_, err = ExecuteRun(context.Background(), `return await parallel([() => agent("x")]);`, nil, Limits{}, Observer{}, func(context.Context, string, AgentOptions) (AgentResult, error) {
		return AgentResult{Completed: true, Value: map[string]any{"bad": func() {}}}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "non-JSON data") {
		t.Fatalf("non-JSON child result was accepted: %v", err)
	}
}

func TestWorkflowConcurrentChildLimit(t *testing.T) {
	var active, peak atomic.Int32
	result, err := ExecuteRun(context.Background(), `
return await parallel([() => agent("a"), () => agent("b"), () => agent("c")]);
`, nil, Limits{MaxConcurrentAgents: 1}, Observer{}, func(_ context.Context, prompt string, _ AgentOptions) (AgentResult, error) {
		current := active.Add(1)
		if current > peak.Load() {
			peak.Store(current)
		}
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		return AgentResult{Completed: true, Value: prompt}, nil
	})
	if err != nil || result.AgentsStarted != 3 || peak.Load() != 1 {
		t.Fatalf("limit was not enforced: result=%#v peak=%d err=%v", result, peak.Load(), err)
	}
}

func TestWorkflowQueuedChildrenStartInScriptOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan string, 3)
	release := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := ExecuteRun(ctx,
			`return await parallel([() => agent("one"), () => agent("two"), () => agent("three")]);`,
			nil, Limits{MaxConcurrentAgents: 1}, Observer{},
			func(ctx context.Context, prompt string, _ AgentOptions) (AgentResult, error) {
				started <- prompt
				index := map[string]int{"one": 0, "two": 1, "three": 2}[prompt]
				select {
				case <-release[index]:
					return AgentResult{Value: prompt, Completed: true}, nil
				case <-ctx.Done():
					return AgentResult{}, ctx.Err()
				}
			})
		done <- outcome{result: result, err: err}
	}()
	for index, want := range []string{"one", "two", "three"} {
		select {
		case got := <-started:
			if got != want {
				t.Fatalf("admission %d = %q, want %q", index, got, want)
			}
		case <-ctx.Done():
			t.Fatal("queued child did not start")
		}
		close(release[index])
	}
	select {
	case got := <-done:
		if got.err != nil || !reflect.DeepEqual(got.result.Value, []any{"one", "two", "three"}) {
			t.Fatalf("result=%#v err=%v", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("workflow did not finish")
	}
}

func TestWorkflowDisposesUnawaitedChildrenBeforeReturn(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := ExecuteRun(ctx, `
agent("dropped");
await agent("barrier");
return "done";
`, nil, Limits{MaxConcurrentAgents: 2}, Observer{}, func(ctx context.Context, prompt string, _ AgentOptions) (AgentResult, error) {
		if prompt == "dropped" {
			close(started)
			<-ctx.Done()
			close(stopped)
			return AgentResult{Completed: false}, nil
		}
		select {
		case <-started:
			return AgentResult{Completed: true, Value: "ready"}, nil
		case <-ctx.Done():
			return AgentResult{}, ctx.Err()
		}
	})
	if err != nil || result.Value != "done" || result.AgentsStarted != 2 {
		t.Fatalf("unexpected result: %#v, %v", result, err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("dropped child was not cleaned up before return")
	}
}

func TestWorkflowDroppedChildProviderFailureDoesNotFailKeptResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var mu sync.Mutex
	var ended []AgentLifecycle
	result, err := ExecuteRunObserved(ctx, `agent("dropped"); return await agent("kept");`, nil, nil,
		Limits{MaxConcurrentAgents: 2}, Observer{
			AgentEnd: func(event AgentLifecycle) {
				mu.Lock()
				ended = append(ended, event)
				mu.Unlock()
			},
		}, func(_ context.Context, prompt string, _ AgentOptions, started func(string)) (AgentResult, error) {
			started("child-" + prompt)
			if prompt == "dropped" {
				return AgentResult{}, errors.New("dropped child failed")
			}
			// Give the owning VM time to settle the dropped promise before the
			// awaited child succeeds, exercising the unobserved-rejection path.
			time.Sleep(40 * time.Millisecond)
			return AgentResult{Value: "kept result", Completed: true}, nil
		})
	if err != nil || result.Value != "kept result" || result.AgentsStarted != 2 {
		t.Fatalf("dropped child changed workflow result: result=%#v err=%v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ended) != 2 || ended[0].ChildID != "child-dropped" || ended[0].Outcome != "failed" || ended[1].ChildID != "child-kept" || ended[1].Outcome != "completed" {
		t.Fatalf("child lifecycle = %+v", ended)
	}
}

func TestWorkflowDroppedOrHandledHookErrorDoesNotFailScript(t *testing.T) {
	for _, script := range []string{
		`agent(""); return await agent("kept");`,
		`try { await agent(""); } catch (_) {} return await agent("kept");`,
		`try { phase(null); } catch (_) {} return await agent("kept");`,
	} {
		result, err := ExecuteRun(context.Background(), script, nil, Limits{}, Observer{},
			func(_ context.Context, prompt string, _ AgentOptions) (AgentResult, error) {
				return AgentResult{Value: prompt, Completed: true}, nil
			})
		if err != nil || result.Value != "kept" || result.AgentsStarted != 1 {
			t.Errorf("script %q: result=%#v err=%v", script, result, err)
		}
	}
}

func TestWorkflowFailurePreservesStartedAgentCount(t *testing.T) {
	result, err := ExecuteRun(context.Background(), `return await agent("work");`, nil,
		Limits{}, Observer{}, func(context.Context, string, AgentOptions) (AgentResult, error) {
			return AgentResult{}, errors.New("child provider unavailable")
		})
	if err == nil || !strings.Contains(err.Error(), "child provider unavailable") {
		t.Fatalf("provider failure = %v", err)
	}
	if result.AgentsStarted != 1 {
		t.Fatalf("failed workflow lost started child count: %+v", result)
	}
}

func TestWorkflowObservedChildLifecyclePairsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var mu sync.Mutex
	var events []AgentLifecycle
	done := make(chan error, 1)
	go func() {
		_, err := ExecuteRunObserved(ctx, `return await agent("wait");`, nil, map[string]any{"name": "cancel-test"},
			Limits{}, Observer{
				AgentStart: func(info AgentLifecycle) { mu.Lock(); events = append(events, info); mu.Unlock() },
				AgentEnd:   func(info AgentLifecycle) { mu.Lock(); events = append(events, info); mu.Unlock() },
			}, func(ctx context.Context, _ string, _ AgentOptions, onStart func(string)) (AgentResult, error) {
				onStart("child-1")
				close(started)
				<-ctx.Done()
				return AgentResult{}, ctx.Err()
			})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("observed child did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("workflow cancellation = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled workflow did not settle")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0].ChildID != "child-1" || events[1].ChildID != "child-1" || events[1].Outcome != "cancelled" {
		t.Fatalf("observed child lifecycle = %+v", events)
	}
}

func TestWorkflowObserverPanicDoesNotChangeScriptResult(t *testing.T) {
	result, err := ExecuteRun(context.Background(), `phase("p"); log("l"); return 42;`, nil, Limits{}, Observer{
		Phase: func(string) { panic("broken progress listener") },
		Log:   func(string) { panic("broken progress listener") },
	}, func(context.Context, string, AgentOptions) (AgentResult, error) { return AgentResult{}, nil })
	if err != nil || result.Value != int64(42) {
		t.Fatalf("observer changed result: %#v, %v", result, err)
	}
}
