package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// ToolPipelineResult is the lossless, execution-local result owned by Go. Value is not another Session fact: the existing journal
// records Content, Meta and the structured failure identity after settlement.
type ToolPipelineResult struct {
	IsError            bool               `json:"isError"`
	Value              json.RawMessage    `json:"value,omitempty"`
	Content            []json.RawMessage  `json:"content"`
	Error              *ToolPipelineError `json:"error,omitempty"`
	Meta               json.RawMessage    `json:"meta,omitempty"`
	AdditionalContexts []json.RawMessage  `json:"additionalContexts,omitempty"`
	ConcludesTurn      bool               `json:"concludesTurn,omitempty"`
}

type ToolPipelineError struct {
	Message string                 `json:"message"`
	Info    *ToolPipelineErrorInfo `json:"info,omitempty"`
}

type ToolPipelineErrorInfo struct {
	Name   string  `json:"name"`
	Code   string  `json:"code"`
	Reason *string `json:"reason,omitempty"`
}

type PreToolDecision struct {
	Kind          string                 `json:"kind"`
	Reason        string                 `json:"reason,omitempty"`
	DisplayReason map[string]string      `json:"displayReason,omitempty"`
	Info          *ToolPipelineErrorInfo `json:"info,omitempty"`
}

type PostToolDecision struct {
	Kind               string                 `json:"kind"`
	Value              json.RawMessage        `json:"value,omitempty"`
	Content            []json.RawMessage      `json:"content,omitempty"`
	Feedback           []json.RawMessage      `json:"feedback,omitempty"`
	Info               *ToolPipelineErrorInfo `json:"info,omitempty"`
	AdditionalContexts []json.RawMessage      `json:"additionalContexts,omitempty"`
}

// ToolPipelineProcessors are Go-internal result processors. They are not
// plugin hooks: the Bridge exposes neither registration nor stage dispatch.
// Go owns preparation, one body invocation, post-processing and settlement.
type ToolPipelineProcessors struct {
	Normalize    func(context.Context, ToolPipelineResult) (ToolPipelineResult, error)
	Project      func(context.Context, ToolPipelineResult) ([]json.RawMessage, error)
	PostExecute  func(context.Context, ToolPipelineResult) (PostToolDecision, error)
	ReplaceValue func(context.Context, json.RawMessage) (ToolPipelineResult, error)
	Finalize     func(context.Context, ToolPipelineResult) ([]json.RawMessage, error)
}

// ToolDispatchOutcome retains the upstream post-result/final-result distinction.
// Preparation and completion are ordered by the batch owner; only Dispatch may
// overlap sibling calls. Immediate preparation failures do not dispatch a body.
type ToolDispatchOutcome struct {
	Result    ToolPipelineResult
	NeedsPost bool
}

type ToolPreparation struct {
	Dispatch bool
	Outcome  ToolDispatchOutcome
}

type ToolInvocation struct {
	ctx        context.Context
	execution  ToolExecution
	hooks      ToolPipelineProcessors
	body       func(context.Context) (ToolPipelineResult, error)
	approve    func(context.Context, ToolPolicyAsk) error
	defaultPre func(context.Context) (PreToolDecision, error)
	dispatched atomic.Bool
	started    atomic.Bool
	trackBody  bool
}

func NewToolInvocation(ctx context.Context, execution ToolExecution, hooks ToolPipelineProcessors,
	body func(context.Context) (ToolPipelineResult, error), approve func(context.Context, ToolPolicyAsk) error,
) *ToolInvocation {
	execution.Arguments = append(json.RawMessage(nil), execution.Arguments...)
	return &ToolInvocation{ctx: ctx, execution: execution, hooks: hooks, body: body, approve: approve}
}

// SetPreExecutePolicy installs the Go execution policy before preparation.
func (p *ToolInvocation) SetPreExecutePolicy(policy func(context.Context) (PreToolDecision, error)) {
	p.defaultPre = policy
}

func (p *ToolInvocation) Prepare() (prepared ToolPreparation) {
	defer func() {
		if value := recover(); value != nil {
			prepared = ToolPreparation{Outcome: ToolDispatchOutcome{Result: pipelineFailure(fmt.Errorf("tool preparation panicked: %v", value))}}
		}
	}()
	if p.ctx.Err() != nil {
		return ToolPreparation{Outcome: ToolDispatchOutcome{Result: pipelineAborted(false)}}
	}
	defaultPre := p.defaultPre
	if defaultPre == nil {
		defaultPre = func(context.Context) (PreToolDecision, error) { return PreToolDecision{Kind: "allow"}, nil }
	}
	decision, err := defaultPre(p.ctx)
	if err != nil {
		return ToolPreparation{Outcome: ToolDispatchOutcome{Result: pipelineFailure(err)}}
	}
	if decision.Kind == "ask" {
		decision, err = p.resolveAsk(decision)
		if err != nil {
			return ToolPreparation{Outcome: ToolDispatchOutcome{Result: pipelineFailure(err)}}
		}
	}
	switch decision.Kind {
	case "allow":
	case "deny":
		return ToolPreparation{Outcome: ToolDispatchOutcome{Result: pipelineDenial(decision.Reason, decision.Info), NeedsPost: true}}
	case "cancel":
		return ToolPreparation{Outcome: ToolDispatchOutcome{Result: pipelineAborted(false), NeedsPost: true}}
	default:
		return ToolPreparation{Outcome: ToolDispatchOutcome{Result: pipelineFailure(fmt.Errorf("invalid pre-execute decision %q", decision.Kind))}}
	}
	if p.ctx.Err() != nil {
		return ToolPreparation{Outcome: ToolDispatchOutcome{Result: pipelineAborted(false), NeedsPost: true}}
	}
	return ToolPreparation{Dispatch: true}
}

func (p *ToolInvocation) resolveAsk(ask PreToolDecision) (PreToolDecision, error) {
	if p.approve == nil || p.execution.SessionID == "" {
		return PreToolDecision{Kind: "deny", Reason: "tool requires approval, but no approval channel is available", Info: &ToolPipelineErrorInfo{Name: "ApprovalError", Code: "APPROVAL_UNAVAILABLE"}}, nil
	}
	err := p.approve(p.ctx, ToolPolicyAsk{Reason: ask.Reason, DisplayReason: ask.DisplayReason})
	if err == nil {
		return PreToolDecision{Kind: "allow"}, nil
	}
	var info interface{ ToolErrorInfo() (string, string) }
	if errors.As(err, &info) {
		_, code := info.ToolErrorInfo()
		if code == "APPROVAL_CANCELLED" && p.ctx.Err() != nil {
			return PreToolDecision{Kind: "cancel"}, nil
		}
		if code == "APPROVAL_REJECTED" || code == "APPROVAL_CANCELLED" || code == "APPROVAL_UNAVAILABLE" {
			return PreToolDecision{Kind: "deny", Reason: err.Error(), Info: &ToolPipelineErrorInfo{Name: "ApprovalError", Code: code}}, nil
		}
	}
	return PreToolDecision{}, err
}

func (p *ToolInvocation) Dispatch() (outcome ToolDispatchOutcome) {
	defer func() {
		if value := recover(); value != nil {
			outcome = ToolDispatchOutcome{Result: pipelineFailure(fmt.Errorf("tool dispatch panicked: %v", value))}
		}
	}()
	// A recorded invocation has exactly one dispatch; no middleware can
	// suppress, replay or replace the selected tool body.
	if !p.dispatched.CompareAndSwap(false, true) {
		return ToolDispatchOutcome{Result: pipelineFailure(errors.New("tool was already dispatched"))}
	}
	if p.ctx.Err() != nil {
		return ToolDispatchOutcome{Result: pipelineAborted(false), NeedsPost: true}
	}
	callCtx := p.ctx
	if p.trackBody {
		callCtx = context.WithValue(callCtx, toolBodyStartedKey{}, &p.started)
	} else {
		p.started.Store(true)
	}
	result, err := p.dispatchBody(callCtx)
	if err != nil {
		if p.ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			result = pipelineAborted(p.started.Load())
		} else {
			result = pipelineFailure(err)
		}
	}
	if p.hooks.Normalize != nil {
		result, err = p.hooks.Normalize(p.ctx, clonePipelineResult(result))
		if err != nil {
			return ToolDispatchOutcome{Result: pipelineFailure(err)}
		}
	}
	if p.ctx.Err() != nil && !result.IsError {
		result = abortedWithPresentation(result, p.started.Load())
	}
	return ToolDispatchOutcome{Result: result, NeedsPost: true}
}

func (p *ToolInvocation) Complete(outcome ToolDispatchOutcome) (result ToolPipelineResult) {
	result = outcome.Result
	defer func() {
		if value := recover(); value != nil {
			result = p.finish(pipelineFailure(fmt.Errorf("tool completion panicked: %v", value)))
		}
	}()
	if !outcome.NeedsPost {
		return p.finish(result)
	}
	if p.hooks.Project != nil {
		content, err := p.hooks.Project(p.ctx, clonePipelineResult(result))
		if err != nil {
			return p.finish(pipelineFailure(err))
		}
		if content != nil {
			result.Content = content
		}
	}
	if p.hooks.PostExecute != nil {
		decision, err := p.hooks.PostExecute(p.ctx, clonePipelineResult(result))
		if err != nil {
			return p.finish(pipelineFailure(err))
		}
		result, err = p.applyPost(result, decision)
		if err != nil {
			return p.finish(pipelineFailure(err))
		}
	}
	if p.ctx.Err() != nil && !result.IsError {
		result = abortedWithPresentation(result, p.started.Load())
	}
	return p.finish(result)
}

func (p *ToolInvocation) applyPost(result ToolPipelineResult, decision PostToolDecision) (ToolPipelineResult, error) {
	switch decision.Kind {
	case "block":
		message := "tool result blocked by post-execute policy"
		var feedback []string
		for _, raw := range decision.Feedback {
			var block struct{ Type, Text string }
			if err := json.Unmarshal(raw, &block); err != nil {
				return ToolPipelineResult{}, err
			}
			if block.Type == "text" {
				feedback = append(feedback, block.Text)
			} else {
				feedback = append(feedback, "["+block.Type+" content]")
			}
		}
		if text := strings.Join(feedback, "\n"); text != "" {
			message = text
		}
		return ToolPipelineResult{IsError: true, Content: decision.Feedback, Error: &ToolPipelineError{Message: message, Info: decision.Info}, AdditionalContexts: decision.AdditionalContexts}, nil
	case "accept":
		if decision.Value != nil && decision.Content != nil {
			return ToolPipelineResult{}, errors.New("tools/post-execute accept decision cannot replace both value and content")
		}
		contexts := append(append([]json.RawMessage(nil), result.AdditionalContexts...), decision.AdditionalContexts...)
		if decision.Value != nil {
			if result.IsError {
				return ToolPipelineResult{}, errors.New("tools/post-execute cannot replace the value of a failed result")
			}
			if p.hooks.ReplaceValue == nil {
				return ToolPipelineResult{}, errors.New("tool output renderer is unavailable")
			}
			var err error
			result, err = p.hooks.ReplaceValue(p.ctx, decision.Value)
			if err != nil {
				return ToolPipelineResult{}, err
			}
		} else if decision.Content != nil {
			result.Content = decision.Content
		}
		result.AdditionalContexts = contexts
		return result, nil
	default:
		return ToolPipelineResult{}, fmt.Errorf("invalid post-execute decision %q", decision.Kind)
	}
}

func (p *ToolInvocation) finish(candidate ToolPipelineResult) (result ToolPipelineResult) {
	result, err := materializePipelineResult(candidate)
	if err != nil {
		result = pipelineFailure(err)
	}
	if p.hooks.Finalize != nil {
		func() {
			defer func() {
				if value := recover(); value != nil {
					result = pipelineFailure(fmt.Errorf("tool finalizer panicked: %v", value))
				}
			}()
			content, finalizeErr := p.hooks.Finalize(p.ctx, clonePipelineResult(result))
			if finalizeErr != nil {
				result = pipelineFailure(finalizeErr)
			} else if content != nil {
				result.Content = content
			}
		}()
	}
	if materialized, err := materializePipelineResult(result); err != nil {
		result = pipelineFailure(err)
	} else {
		result = materialized
	}
	return result
}

func (p *ToolInvocation) dispatchBody(ctx context.Context) (result ToolPipelineResult, err error) {
	defer func() {
		if value := recover(); value != nil {
			result, err = pipelineFailure(fmt.Errorf("tool body panicked: %v", value)), nil
		}
	}()
	if p.body == nil {
		return ToolPipelineResult{}, errors.New("tool body is unavailable")
	}
	return p.body(ctx)
}

func materializePipelineResult(result ToolPipelineResult) (ToolPipelineResult, error) {
	if result.IsError {
		if result.Error == nil {
			return ToolPipelineResult{}, errors.New("failed tool result lacks an error")
		}
		result.Value, result.ConcludesTurn = nil, false
	} else if !json.Valid(result.Value) {
		return ToolPipelineResult{}, errors.New("successful tool result lacks a JSON value")
	}
	if result.Content == nil {
		result.Content = []json.RawMessage{}
	}
	data, err := json.Marshal(result)
	if err != nil {
		return ToolPipelineResult{}, err
	}
	var detached ToolPipelineResult
	err = json.Unmarshal(data, &detached)
	return detached, err
}

func clonePipelineResult(result ToolPipelineResult) ToolPipelineResult {
	// Invalid values are retained for the owning stage to diagnose rather than
	// accidentally converted to a valid empty result by a failed clone.
	data, err := json.Marshal(result)
	if err != nil {
		return result
	}
	var detached ToolPipelineResult
	if json.Unmarshal(data, &detached) != nil {
		return result
	}
	return detached
}

func pipelineFailure(err error) ToolPipelineResult {
	result := pipelineDenial(err.Error(), nil)
	var info interface{ ToolErrorInfo() (string, string) }
	if errors.As(err, &info) {
		name, code := info.ToolErrorInfo()
		if code != "" {
			result.Error.Info = &ToolPipelineErrorInfo{Name: name, Code: code}
		}
	}
	return result
}

func pipelineDenial(reason string, info *ToolPipelineErrorInfo) ToolPipelineResult {
	block, _ := json.Marshal(map[string]string{"type": "text", "text": "Error: " + reason})
	return ToolPipelineResult{IsError: true, Content: []json.RawMessage{block}, Error: &ToolPipelineError{Message: reason, Info: info}}
}

func pipelineAborted(started bool) ToolPipelineResult {
	message, code := "tool call aborted before dispatch", "ABORTED_BEFORE_DISPATCH"
	if started {
		message, code = "tool call aborted", "ABORTED"
	}
	return pipelineDenial(message, &ToolPipelineErrorInfo{Name: "AbortError", Code: code})
}

func abortedWithPresentation(result ToolPipelineResult, started bool) ToolPipelineResult {
	aborted := pipelineAborted(started)
	aborted.AdditionalContexts = result.AdditionalContexts
	return aborted
}

// Native dispatch includes argument validation before the selected business
// function. Its body marker is set only after that validation succeeds.
type toolBodyStartedKey struct{}

func markToolBodyStarted(ctx context.Context) {
	if started, ok := ctx.Value(toolBodyStartedKey{}).(*atomic.Bool); ok {
		started.Store(true)
	}
}
