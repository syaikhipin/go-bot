package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-bot/harness/interaction"
	"go-bot/harness/session"
	"go-bot/internal/modeldata"
)

type ToolExecution struct {
	Unavailable   bool                    `json:"unavailable,omitempty"`
	SessionID     string                  `json:"sessionId"`
	CWD           string                  `json:"cwd"`
	CallID        string                  `json:"callId"`
	Name          string                  `json:"name"`
	Arguments     json.RawMessage         `json:"arguments"`
	Turn          uint64                  `json:"turn"`
	Step          uint64                  `json:"step"`
	SandboxPolicy *ExecutionSandboxPolicy `json:"sandboxPolicy,omitempty"`
}

type ToolPolicyAsk struct {
	Reason        string            `json:"reason,omitempty"`
	DisplayReason map[string]string `json:"displayReason,omitempty"`
}

type NativePreToolPolicy func(context.Context, ToolExecution) (PreToolDecision, error)
type ToolInvocationFactory func(context.Context, ToolExecution, func(context.Context) modeldata.ToolResult, func(context.Context, ToolPolicyAsk) error) (*ToolInvocation, error)
type ToolInvocationSource func(context.Context, string) (ToolInvocationFactory, error)

func (a *NativeLoop) SetToolQuestions(questions *interaction.NativeQuestions) {
	a.questions = questions
}

func (a *NativeLoop) SetToolInvocationSource(source ToolInvocationSource) {
	a.toolInvocationSource = source
}
func (a *NativeLoop) SetToolPreExecutePolicy(policy NativePreToolPolicy) { a.preExecutePolicy = policy }
func (a *NativeLoop) SetToolPolicyApprover(approve func(context.Context, ToolExecution, ToolPolicyAsk) error) {
	a.toolPolicyApprover = approve
}
func (a *NativeLoop) refreshToolInvocationSource(ctx context.Context) error {
	if a.toolInvocationSource == nil {
		a.nativeTools.mu.Lock()
		a.toolInvocationFactory = nil
		a.nativeTools.mu.Unlock()
		return nil
	}
	factory, err := a.toolInvocationSource(ctx, a.session.Header().ID)
	if err != nil {
		return fmt.Errorf("resolve tool execution policy: %w", err)
	}
	a.nativeTools.mu.Lock()
	a.toolInvocationFactory = factory
	a.nativeTools.mu.Unlock()
	return nil
}

type nativePreparedTool struct {
	call        modeldata.ToolRequest
	invocation  *ToolInvocation
	preparation ToolPreparation
	fallback    *modeldata.ToolResult
	canonical   *ToolPipelineResult
	settler     SettledTool
	settled     bool
	postResult  func(modeldata.ToolRequest, *modeldata.ToolResult)
	output      func(modeldata.ToolResult) modeldata.ToolResult
}

func (a *nativeTools) prepareNativeTool(ctx context.Context, turn, step uint64, call modeldata.ToolRequest) *nativePreparedTool {
	prepared := &nativePreparedTool{call: call, postResult: a.postResult}
	args := json.RawMessage(call.Arguments)
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	// Match upstream argument parsing: malformed JSON is a string that policy
	// can inspect; the tool's original argument diagnostic remains its own job.
	if !json.Valid(args) {
		args, _ = json.Marshal(call.Arguments)
	}
	header := a.header()
	tool, schema, resolveErr := a.lookupTool(ctx, call.Name)
	if resolveErr != nil {
		result := nativePolicyFailure(call, resolveErr)
		prepared.fallback = &result
		return prepared
	}
	prepared.settler, _ = tool.(SettledTool)
	prepared.output = prepareToolOutput(ctx, a.header().CWD, tool, call)
	execution := ToolExecution{Unavailable: tool == nil, SessionID: header.ID, CWD: header.CWD, CallID: call.ID, Name: call.Name, Arguments: append(json.RawMessage(nil), args...), Turn: turn, Step: step}
	execution.SandboxPolicy = a.executionSandboxPolicy()
	ctx = withToolPosition(ctx, turn, step)
	ctx = withToolCallID(ctx, call.ID)
	ctx = context.WithValue(ctx, toolQuestionsKey{}, a.questions)
	dispatch := func(ctx context.Context) modeldata.ToolResult {
		return a.executeNativeToolDirect(ctx, turn, step, call, tool, schema)
	}
	approve := func(ctx context.Context, ask ToolPolicyAsk) error {
		if a.toolPolicyApprover == nil {
			return toolPolicyError{"APPROVAL_UNAVAILABLE", "No interactive approval service is available"}
		}
		return a.toolPolicyApprover(ctx, execution, ask)
	}
	var err error
	if a.toolInvocationFactory != nil {
		prepared.invocation, err = a.toolInvocationFactory(ctx, execution, dispatch, approve)
	} else {
		prepared.invocation = NewToolInvocation(ctx, execution, ToolPipelineProcessors{}, func(ctx context.Context) (ToolPipelineResult, error) { return EncodeToolPipelineResult(dispatch(ctx)) }, approve)
	}
	if err != nil || prepared.invocation == nil {
		if err == nil {
			err = fmt.Errorf("tool execution factory returned no invocation")
		}
		result := nativePolicyFailure(call, err)
		prepared.fallback = &result
		return prepared
	}
	if a.preExecutePolicy != nil {
		prepared.invocation.SetPreExecutePolicy(func(ctx context.Context) (PreToolDecision, error) { return a.preExecutePolicy(ctx, execution) })
	}
	prepared.invocation.trackBody = true
	prepared.preparation = prepared.invocation.Prepare()
	return prepared
}

func (p *nativePreparedTool) dispatch() ToolDispatchOutcome {
	if p.fallback != nil {
		return ToolDispatchOutcome{}
	}
	if !p.preparation.Dispatch {
		return p.preparation.Outcome
	}
	return p.invocation.Dispatch()
}

func (p *nativePreparedTool) settle(success bool) {
	if !p.settled && p.settler != nil {
		p.settler.SettleResult(p.call.ID, success)
	}
	p.settled = true
}
func (p *nativePreparedTool) complete(outcome ToolDispatchOutcome) (result modeldata.ToolResult) {
	if p.fallback != nil {
		return *p.fallback
	}
	canonical := p.invocation.Complete(outcome)
	p.canonical = &canonical
	decoded, err := DecodeToolPipelineResult(canonical)
	if err == nil {
		for _, message := range decoded.AdditionalContexts {
			var fields struct{ ID, Role string }
			if json.Unmarshal(message, &fields) != nil || fields.ID == "" || fields.Role != "user" {
				err = fmt.Errorf("tool policy context must be a user message with an identity")
				break
			}
			if err = sessionformat.ValidateV4Rows([]sessionformat.Event{{Type: "user/message", Data: message}}); err != nil {
				break
			}
		}
	}
	if err != nil {
		p.canonical = nil
		return nativePolicyFailure(p.call, err)
	}
	decoded.ID, decoded.Name = p.call.ID, p.call.Name
	if outcome.NeedsPost && p.output != nil {
		decoded = p.output(decoded)
		updated, encodeErr := EncodeToolPipelineResult(decoded)
		if encodeErr != nil {
			p.canonical = nil
			return nativePolicyFailure(p.call, encodeErr)
		}
		// Retention changes presentation only; preserve the canonical value.
		updated.Value = canonical.Value
		canonical, err = materializePipelineResult(updated)
		if err != nil {
			p.canonical = nil
			return nativePolicyFailure(p.call, err)
		}
		decoded, err = DecodeToolPipelineResult(canonical)
		if err != nil {
			p.canonical = nil
			return nativePolicyFailure(p.call, err)
		}
		decoded.ID, decoded.Name = p.call.ID, p.call.Name
	}
	if outcome.NeedsPost && p.postResult != nil {
		p.postResult(p.call, &decoded)
		p.canonical.AdditionalContexts = decoded.AdditionalContexts
	}
	return decoded
}
func nativePolicyFailure(call modeldata.ToolRequest, err error) modeldata.ToolResult {
	return modeldata.ToolResult{ID: call.ID, Name: call.Name, IsError: true, Code: "TOOL_POLICY_ERROR", Content: "Error: " + err.Error()}
}
func (a *nativeTools) executeNativeTool(ctx context.Context, turn, step uint64, call modeldata.ToolRequest) modeldata.ToolResult {
	p := a.prepareNativeTool(ctx, turn, step, call)
	defer p.settle(false)
	result := p.complete(p.dispatch())
	p.settle(!result.IsError)
	return result
}

type toolPolicyError struct{ code, message string }

func (e toolPolicyError) Error() string                   { return e.message }
func (e toolPolicyError) ToolErrorInfo() (string, string) { return "ToolPolicyError", e.code }
