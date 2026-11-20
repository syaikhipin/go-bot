package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"go-bot/harness/agent"
	"go-bot/internal/modeldata"
)

type hostBodyKey struct{}
type hostBody struct {
	execution agent.ToolExecution
	approve   func(context.Context, agent.ToolPolicyAsk) error
}
type hostLiveCall struct {
	owner string
	body  hostBody
	ctx   context.Context
}

func hostToolInvocation(ctx context.Context, execution agent.ToolExecution, dispatch func(context.Context) modeldata.ToolResult, approve func(context.Context, agent.ToolPolicyAsk) error) (*agent.ToolInvocation, error) {
	return agent.NewToolInvocation(ctx, execution, agent.ToolPipelineProcessors{}, func(callCtx context.Context) (agent.ToolPipelineResult, error) {
		callCtx = context.WithValue(callCtx, hostBodyKey{}, hostBody{execution: execution, approve: approve})
		return agent.EncodeToolPipelineResult(dispatch(callCtx))
	}, approve), nil
}

func (r *hostBridge) beginCall(ctx context.Context, tool *hostTool) (map[string]any, func()) {
	body, ok := ctx.Value(hostBodyKey{}).(hostBody)
	if !ok {
		body.execution = agent.ToolExecution{SessionID: tool.sessionID, CWD: tool.cwd, Name: tool.Spec().Name}
		body.execution.CallID, _ = agent.ToolCallIDFromContext(ctx)
	}
	token := uuid.NewString()
	r.invocations.Store(token, &hostLiveCall{owner: tool.row.Owner, body: body, ctx: ctx})
	return map[string]any{"id": body.execution.CallID, "sessionId": tool.sessionID, "cwd": tool.cwd, "workspace": tool.workspace, "invocation": token}, func() { r.invocations.Delete(token) }
}

func (r *hostBridge) approvalCall(ctx context.Context, owner, token string, input json.RawMessage) (any, error) {
	value, ok := r.invocations.Load(token)
	if !ok {
		return nil, failure("UNAVAILABLE", "The tool invocation is not active")
	}
	live := value.(*hostLiveCall)
	if live.owner != owner {
		return nil, failure("INVALID_ARGUMENT", "The invocation belongs to another plugin")
	}
	if live.ctx.Err() != nil {
		return "cancelled", nil
	}
	var request struct {
		Reason string `json:"reason"`
		Detail string `json:"detail,omitempty"`
	}
	if err := decodeHostJSON(input, &request); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Reason) == "" {
		return nil, failure("INVALID_ARGUMENT", "An approval reason is required")
	}
	if live.body.approve == nil {
		return "unavailable", nil
	}
	callCtx, cancel := context.WithCancel(live.ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	reason := request.Reason
	if request.Detail != "" {
		reason += "\n" + request.Detail
	}
	err := live.body.approve(callCtx, agent.ToolPolicyAsk{Reason: reason})
	if err == nil {
		return "allowed", nil
	}
	var detail interface{ ToolErrorInfo() (string, string) }
	if errors.As(err, &detail) {
		_, code := detail.ToolErrorInfo()
		switch code {
		case "APPROVAL_REJECTED":
			return "rejected", nil
		case "APPROVAL_UNAVAILABLE":
			return "unavailable", nil
		case "APPROVAL_CANCELLED":
			return "cancelled", nil
		}
	}
	if callCtx.Err() != nil {
		return "cancelled", nil
	}
	return nil, err
}
