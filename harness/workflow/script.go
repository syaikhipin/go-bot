package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"
)

// Delegate is the only external capability available to a workflow script.
// The eventual Subagent adapter owns child admission, limits, and cleanup.
type Delegate func(context.Context, string) (any, error)

type AgentOptions struct {
	Label, Phase, Provider, Model string
	Schema                        map[string]any
}

// Completed=false is an ordinary child failure and becomes null in the script.
// A non-nil error means the child provider itself failed and is fatal.
type AgentResult struct {
	Value     any
	Completed bool
}

type DelegateWithOptions func(context.Context, string, AgentOptions) (AgentResult, error)

type Limits struct {
	MaxConcurrentAgents int
	MaxTotalAgents      int
	MaxItemsPerCall     int
	SyncTimeout         time.Duration
}

func (l Limits) defaults() Limits {
	if l.MaxConcurrentAgents <= 0 {
		l.MaxConcurrentAgents = runtime.NumCPU()
	}
	if l.MaxConcurrentAgents < 1 {
		l.MaxConcurrentAgents = 1
	}
	if l.MaxTotalAgents <= 0 {
		l.MaxTotalAgents = 1000
	}
	if l.MaxItemsPerCall <= 0 {
		l.MaxItemsPerCall = 4096
	}
	if l.SyncTimeout <= 0 {
		l.SyncTimeout = 5 * time.Second
	}
	return l
}

type Observer struct {
	Phase      func(string)
	Log        func(string)
	AgentStart func(AgentLifecycle)
	AgentEnd   func(AgentLifecycle)
}

type AgentLifecycle struct {
	Seq     int
	Label   string
	Phase   string
	ChildID string
	Outcome string
}

type DelegateObserved func(context.Context, string, AgentOptions, func(string)) (AgentResult, error)

type Result struct {
	Value         any
	AgentsStarted int
}

// Compile lowers an async script body to syntax the embedded VM implements.
// The body contract matches DeepSeek Harness: top-level await is valid inside
// the implicit async function, and a final return value is the result.
func Compile(script string) (string, error) {
	if strings.TrimSpace(script) == "" || len(script) > 256<<10 {
		return "", errors.New("workflow script must be nonempty and at most 256 KiB")
	}
	source := "async function __workflow_main__() {\n" + script + "\n}\n__workflow_main__();"
	result := api.Transform(source, api.TransformOptions{Loader: api.LoaderJS, Target: api.ES2016, LogLevel: api.LogLevelSilent, Sourcefile: "workflow.js"})
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("workflow script parse: %s", result.Errors[0].Text)
	}
	return string(result.Code), nil
}

const hooks = `
(function(hostAgent, hostPhase, hostLog, maxItems) {
  const fatal = new WeakSet();
  function fail(message) {
    const error = new TypeError(message);
    fatal.add(error);
    throw error;
  }
  async function agent(prompt, opts) {
    try { return await hostAgent(prompt, opts); }
    catch (error) { if (error && typeof error === 'object') fatal.add(error); throw error; }
  }
  async function parallel(thunks) {
    if (!Array.isArray(thunks)) fail('parallel() requires an array of functions');
    if (thunks.length > maxItems) fail('parallel() exceeded the per-call item cap');
    for (const thunk of thunks) if (typeof thunk !== 'function') fail('parallel() items must be functions');
    return Promise.all(thunks.map(async (thunk) => {
      try { return await thunk(); } catch (error) { if (fatal.has(error)) throw error; return null; }
    }));
  }
  async function pipeline(items, ...stages) {
    if (!Array.isArray(items)) fail('pipeline() requires an items array');
    if (items.length > maxItems) fail('pipeline() exceeded the per-call item cap');
    if (stages.length === 0 || stages.some(stage => typeof stage !== 'function'))
      fail('pipeline() requires one or more stage functions');
    return Promise.all(items.map(async (item, index) => {
      let value = item;
      for (const stage of stages) {
        try { value = await stage(value, item, index); }
        catch (error) { if (fatal.has(error)) throw error; return null; }
      }
      return value;
    }));
  }
  function phase(title) {
    try { return hostPhase(title); }
    catch (error) { if (error && typeof error === 'object') fatal.add(error); throw error; }
  }
  function log(message) {
    try { return hostLog(message); }
    catch (error) { if (error && typeof error === 'object') fatal.add(error); throw error; }
  }
  function materialize(value, root, seen = new Set()) {
    const kind = typeof value;
    if (value === null || kind === 'string' || kind === 'boolean') return value;
    if (kind === 'number' && Number.isFinite(value)) return value;
    if (kind !== 'object') throw new TypeError(root + ' is not plain JSON data');
    if (seen.has(value)) throw new TypeError(root + ' contains a cycle');
    seen.add(value);
    try {
      if (Array.isArray(value)) {
        const out = [];
        for (let i = 0; i < value.length; i++) {
          if (!(i in value)) throw new TypeError(root + '[' + i + '] is sparse');
          out.push(materialize(value[i], root + '[' + i + ']', seen));
        }
        for (const key of Object.keys(value)) {
          if (!/^(0|[1-9][0-9]*)$/.test(key) || Number(key) >= value.length)
            throw new TypeError(root + ' has extra array properties');
        }
        if (Object.getOwnPropertySymbols(value).length) throw new TypeError(root + ' has symbol properties');
        return out;
      }
      const proto = Object.getPrototypeOf(value);
      if (proto !== null && Object.getPrototypeOf(proto) !== null)
        throw new TypeError(root + ' has an exotic prototype');
      if (Object.getOwnPropertySymbols(value).length) throw new TypeError(root + ' has symbol properties');
      const out = Object.create(null);
      for (const key of Object.keys(value)) out[key] = materialize(value[key], root + '.' + key, seen);
      return out;
    } finally { seen.delete(value); }
  }
  return { agent, parallel, pipeline, phase, log, materialize, markFatal: error => fatal.add(error) };
})
`

type completion func(*goja.Runtime) error

// Execute is a compatibility wrapper for simple child delegates.
func Execute(ctx context.Context, script string, args map[string]any, delegate Delegate) (any, error) {
	if delegate == nil {
		return nil, errors.New("workflow delegate is unavailable")
	}
	result, err := ExecuteRun(ctx, script, args, Limits{}, Observer{}, func(ctx context.Context, prompt string, _ AgentOptions) (AgentResult, error) {
		value, childErr := delegate(ctx, prompt)
		return AgentResult{Value: value, Completed: childErr == nil}, nil
	})
	return result.Value, err
}

// ExecuteRun evaluates one script in an isolated VM. It exposes no Node,
// filesystem, network, or timer globals. All VM access stays on this goroutine;
// Delegate completions are settled via a channel on the owning VM goroutine.
func ExecuteRun(ctx context.Context, script string, args map[string]any, limits Limits, observer Observer, delegate DelegateWithOptions) (Result, error) {
	if delegate == nil {
		return Result{}, errors.New("workflow delegate is unavailable")
	}
	return ExecuteRunObserved(ctx, script, args, nil, limits, observer,
		func(ctx context.Context, prompt string, options AgentOptions, _ func(string)) (AgentResult, error) {
			return delegate(ctx, prompt, options)
		})
}

// ExecuteRunObserved adds the child identity/lifecycle tap used by the
// model-facing tool recorder. The child provider calls started only after it
// has published the child's durable Session and parent discovery fact.
func ExecuteRunObserved(ctx context.Context, script string, args, meta map[string]any, limits Limits, observer Observer, delegate DelegateObserved) (Result, error) {
	compiled, err := Compile(script)
	if err != nil {
		return Result{}, err
	}
	if delegate == nil {
		return Result{}, errors.New("workflow delegate is unavailable")
	}
	limits = limits.defaults()
	vm := goja.New()
	completions := make(chan completion, limits.MaxConcurrentAgents)
	var slotMu sync.Mutex
	var activeSlots int
	var queuedSlots []chan struct{}
	// Reserve on the VM goroutine, in script call order. Letting child goroutines
	// race on a semaphore would make queued agent() admission scheduler-dependent.
	reserveSlot := func() <-chan struct{} {
		slotMu.Lock()
		defer slotMu.Unlock()
		ready := make(chan struct{})
		if activeSlots < limits.MaxConcurrentAgents {
			activeSlots++
			close(ready)
		} else {
			queuedSlots = append(queuedSlots, ready)
		}
		return ready
	}
	releaseSlot := func() {
		slotMu.Lock()
		defer slotMu.Unlock()
		if len(queuedSlots) > 0 {
			next := queuedSlots[0]
			queuedSlots[0] = nil
			queuedSlots = queuedSlots[1:]
			close(next)
			return
		}
		activeSlots--
	}
	childCtx, cancelChildren := context.WithCancel(ctx)
	var children sync.WaitGroup
	defer func() {
		cancelChildren()
		children.Wait()
	}()
	if args == nil {
		args = map[string]any{}
	}
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		return Result{}, fmt.Errorf("workflow args are not JSON data: %w", err)
	}
	var safeArgs any
	if err := json.Unmarshal(encodedArgs, &safeArgs); err != nil {
		return Result{}, err
	}
	if err := vm.Set("args", safeArgs); err != nil {
		return Result{}, err
	}
	if meta == nil {
		meta = map[string]any{}
	}
	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return Result{}, fmt.Errorf("workflow meta is not JSON data: %w", err)
	}
	var safeMeta any
	if err := json.Unmarshal(encodedMeta, &safeMeta); err != nil {
		return Result{}, err
	}
	if err := vm.Set("meta", safeMeta); err != nil {
		return Result{}, err
	}
	var started int
	var currentPhase string
	var materialize goja.Callable
	var markFatal goja.Callable
	callFatal := func(message string) {
		panic(vm.NewTypeError(message))
	}
	hostAgent := func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			callFatal("agent() requires a non-empty prompt string")
		}
		prompt, ok := call.Argument(0).Export().(string)
		if !ok || prompt == "" {
			callFatal("agent() requires a non-empty prompt string")
		}
		options := AgentOptions{Phase: currentPhase}
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Argument(1)) {
			value, err := materialize(goja.Undefined(), call.Argument(1), vm.ToValue("agent() options"))
			if err != nil {
				callFatal("agent() options are not plain JSON data: " + err.Error())
			}
			record, ok := value.Export().(map[string]any)
			if !ok {
				callFatal("agent() options must be an object")
			}
			for key, raw := range record {
				switch key {
				case "label", "phase", "provider", "model":
					text, ok := raw.(string)
					if !ok {
						callFatal("agent() option " + key + " must be a string")
					}
					switch key {
					case "label":
						options.Label = text
					case "phase":
						options.Phase = text
					case "provider":
						options.Provider = text
					case "model":
						options.Model = text
					}
				case "schema":
					schema, ok := raw.(map[string]any)
					if !ok || validateSchema(schema, true) != nil {
						callFatal("agent() schema is outside the supported object-rooted subset")
					}
					options.Schema = schema
				default:
					callFatal("agent() option " + key + " is not supported")
				}
			}
		}
		if started >= limits.MaxTotalAgents {
			callFatal(fmt.Sprintf("workflow reached the total agent cap (%d)", limits.MaxTotalAgents))
		}
		started++
		seq := started
		if options.Label == "" {
			options.Label, _, _ = strings.Cut(prompt, "\n")
			characters := []rune(options.Label)
			if len(characters) > 48 {
				options.Label = string(characters[:47]) + "…"
			}
		}
		promise, resolve, reject := vm.NewPromise()
		ready := reserveSlot()
		children.Add(1)
		go func() {
			defer children.Done()
			select {
			case <-ready:
			case <-childCtx.Done():
				return
			}
			defer releaseSlot()
			if childCtx.Err() != nil {
				return
			}
			var childID string
			value, callErr := delegate(childCtx, prompt, options, func(id string) {
				if id == "" || childID != "" {
					return
				}
				childID = id
				if observer.AgentStart != nil {
					notifyAgentObserver(observer.AgentStart, AgentLifecycle{Seq: seq, Label: options.Label, Phase: options.Phase, ChildID: id})
				}
			})
			if childID != "" && observer.AgentEnd != nil {
				outcome := "failed"
				if childCtx.Err() != nil {
					outcome = "cancelled"
				} else if callErr == nil && value.Completed {
					outcome = "completed"
				}
				notifyAgentObserver(observer.AgentEnd, AgentLifecycle{Seq: seq, Label: options.Label, Phase: options.Phase, ChildID: childID, Outcome: outcome})
			}
			settle := func(vm *goja.Runtime) error {
				if callErr != nil {
					jsErr := vm.NewGoError(fmt.Errorf("child provider failed: %w", callErr))
					_, _ = markFatal(goja.Undefined(), jsErr)
					return reject(jsErr)
				}
				if !value.Completed {
					return resolve(nil)
				}
				// Child results cross a JSON boundary in the original engine.
				// Never hand a Go object or callable directly to the guest VM.
				encoded, err := json.Marshal(value.Value)
				if err != nil {
					jsErr := vm.NewGoError(fmt.Errorf("child provider returned non-JSON data: %w", err))
					_, _ = markFatal(goja.Undefined(), jsErr)
					return reject(jsErr)
				}
				var safeValue any
				if err := json.Unmarshal(encoded, &safeValue); err != nil {
					return err
				}
				return resolve(safeValue)
			}
			select {
			case completions <- settle:
			case <-childCtx.Done():
			}
		}()
		return vm.ToValue(promise)
	}
	hostPhase := func(call goja.FunctionCall) goja.Value {
		value, ok := call.Argument(0).Export().(string)
		if !ok || value == "" {
			callFatal("phase() requires a non-empty title string")
		}
		currentPhase = value
		if observer.Phase != nil {
			notifyObserver(observer.Phase, value)
		}
		return goja.Undefined()
	}
	hostLog := func(call goja.FunctionCall) goja.Value {
		value, ok := call.Argument(0).Export().(string)
		if !ok {
			callFatal("log() requires a message string")
		}
		if observer.Log != nil {
			notifyObserver(observer.Log, value)
		}
		return goja.Undefined()
	}
	hookFactory, err := vm.RunString(hooks)
	if err != nil {
		return Result{}, err
	}
	factory, ok := goja.AssertFunction(hookFactory)
	if !ok {
		return Result{}, errors.New("workflow hooks are not callable")
	}
	hookObject, err := factory(goja.Undefined(), vm.ToValue(hostAgent), vm.ToValue(hostPhase), vm.ToValue(hostLog), vm.ToValue(limits.MaxItemsPerCall))
	if err != nil {
		return Result{}, err
	}
	hookValues := hookObject.ToObject(vm)
	for _, name := range []string{"agent", "parallel", "pipeline", "phase", "log"} {
		if err := vm.Set(name, hookValues.Get(name)); err != nil {
			return Result{}, err
		}
	}
	materialize, ok = goja.AssertFunction(hookValues.Get("materialize"))
	if !ok {
		return Result{}, errors.New("workflow JSON materializer is unavailable")
	}
	markFatal, ok = goja.AssertFunction(hookValues.Get("markFatal"))
	if !ok {
		return Result{}, errors.New("workflow fatal marker is unavailable")
	}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt(ctx.Err())
		case <-stopped:
		}
	}()
	initialDone := make(chan struct{})
	timerDone := make(chan struct{})
	initialTimer := time.NewTimer(limits.SyncTimeout)
	defer initialTimer.Stop()
	go func() {
		defer close(timerDone)
		select {
		case <-initialTimer.C:
			vm.Interrupt(errors.New("workflow initial synchronous slice exceeded its limit"))
		case <-initialDone:
		}
	}()
	value, err := vm.RunString(compiled)
	close(initialDone)
	<-timerDone
	vm.ClearInterrupt()
	if err != nil {
		if ctx.Err() != nil {
			return Result{AgentsStarted: started}, ctx.Err()
		}
		return Result{AgentsStarted: started}, err
	}
	promise, ok := value.Export().(*goja.Promise)
	if !ok {
		return Result{AgentsStarted: started}, errors.New("workflow script did not yield a Promise")
	}
	for promise.State() == goja.PromiseStatePending {
		if err := ctx.Err(); err != nil {
			return Result{AgentsStarted: started}, err
		}
		select {
		case <-ctx.Done():
			return Result{AgentsStarted: started}, ctx.Err()
		case settle := <-completions:
			if err := ctx.Err(); err != nil {
				return Result{AgentsStarted: started}, err
			}
			if err := settle(vm); err != nil {
				return Result{AgentsStarted: started}, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{AgentsStarted: started}, err
	}
	if promise.State() == goja.PromiseStateRejected {
		return Result{AgentsStarted: started}, fmt.Errorf("workflow script failed: %s", promise.Result().String())
	}
	if goja.IsUndefined(promise.Result()) || goja.IsNull(promise.Result()) {
		return Result{AgentsStarted: started}, nil
	}
	safe, err := materialize(goja.Undefined(), promise.Result(), vm.ToValue("workflow result"))
	if err != nil {
		return Result{AgentsStarted: started}, fmt.Errorf("workflow result is not plain JSON data: %w", err)
	}
	return Result{Value: safe.Export(), AgentsStarted: started}, nil
}

// Observers are telemetry, not script control. A broken progress listener may
// not change the workflow's result or stop peer/child work.
func notifyObserver(callback func(string), value string) {
	defer func() { _ = recover() }()
	callback(value)
}

func notifyAgentObserver(callback func(AgentLifecycle), value AgentLifecycle) {
	defer func() { _ = recover() }()
	callback(value)
}
