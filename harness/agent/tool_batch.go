package agent

import (
	"context"
	"errors"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"go-bot/harness/interaction"
	sessionformat "go-bot/harness/session"
	"go-bot/internal/modeldata"
	"sync"
)

// nativeTools is the Go-owned tool execution context of an Agent. There is no
// plugin Agent proxy, arbitrary dispatch API, or second Session store.
type nativeTools struct {
	mu                    sync.RWMutex
	header                func() sessionformat.Header
	questions             *interaction.NativeQuestions
	sandboxMode           func() string
	tools                 map[string]Tool
	toolSchemas           map[string]*jsonschema.Schema
	parallelSafe          map[string]bool
	toolInvocationFactory ToolInvocationFactory
	toolPolicyApprover    func(context.Context, ToolExecution, ToolPolicyAsk) error
	preExecutePolicy      NativePreToolPolicy
	postResult            func(modeldata.ToolRequest, *modeldata.ToolResult)
	maxParallelToolCalls  int
}

func (a *nativeTools) lookupTool(_ context.Context, name string) (Tool, *jsonschema.Schema, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.tools[name], a.toolSchemas[name], nil
}

type batchCallbacks struct {
	parallel func(modeldata.ToolRequest) bool
	start    func(int, modeldata.ToolRequest) (*modeldata.ToolResult, error)
	prepare  func(modeldata.ToolRequest) *nativePreparedTool
	commit   func(int, modeldata.ToolResult, *ToolPipelineResult) error
}

func runToolBatch(calls []modeldata.ToolRequest, limit int, callbacks batchCallbacks) error {
	type completion struct {
		index   int
		outcome ToolDispatchOutcome
	}
	completed := make(chan completion, len(calls))
	prepared := make([]*nativePreparedTool, len(calls))
	results := make([]modeldata.ToolResult, len(calls))
	outcomes := make([]ToolDispatchOutcome, len(calls))
	ready := make([]bool, len(calls))
	nextStart, nextCommit, running := 0, 0, 0
	exclusive := false
	if limit < 1 {
		limit = 1
	}
	defer func() {
		for running > 0 {
			<-completed
			running--
		}
		for _, call := range prepared {
			if call != nil && call.invocation != nil {
				call.settle(false)
			}
		}
	}()
	commitReady := func() error {
		for nextCommit < len(calls) && ready[nextCommit] {
			i := nextCommit
			var canonical *ToolPipelineResult
			if prepared[i] != nil {
				results[i] = prepared[i].complete(outcomes[i])
				canonical = prepared[i].canonical
			}
			if err := callbacks.commit(i, results[i], canonical); err != nil {
				return err
			}
			if prepared[i] != nil {
				prepared[i].settle(!results[i].IsError)
			}
			nextCommit++
		}
		return nil
	}
	fill := func() error {
		for nextStart < len(calls) && running < limit && !exclusive {
			i := nextStart
			call := calls[i]
			parallel := callbacks.parallel(call)
			if !parallel && running > 0 {
				break
			}
			skip, err := callbacks.start(i, call)
			if err != nil {
				return err
			}
			nextStart++
			if skip != nil {
				results[i] = *skip
				ready[i] = true
			} else {
				prepared[i] = callbacks.prepare(call)
				if prepared[i].fallback != nil || !prepared[i].preparation.Dispatch {
					outcomes[i] = prepared[i].dispatch()
					ready[i] = true
				} else {
					running++
					exclusive = !parallel
					go func(index int) { completed <- completion{index, prepared[index].dispatch()} }(i)
				}
			}
			if err := commitReady(); err != nil {
				return err
			}
		}
		return nil
	}
	if err := fill(); err != nil {
		return err
	}
	for running > 0 {
		done := <-completed
		running--
		exclusive = false
		outcomes[done.index], ready[done.index] = done.outcome, true
		if err := commitReady(); err != nil {
			return err
		}
		if err := fill(); err != nil {
			return err
		}
	}
	if nextCommit != len(calls) {
		return errors.New("tool scheduler left uncommitted results")
	}
	return nil
}
