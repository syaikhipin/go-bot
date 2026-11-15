package agent

import (
	"fmt"

	"go-harness/internal/modeldata"
)

const (
	maxDurableToolContentBytes = 1 << 20
	maxDurableToolErrorBytes   = 8 << 10
	maxModelContentBytes       = 1 << 20
	maxModelToolCalls          = 128
	maxModelToolArgumentsBytes = 64 << 10
	maxModelToolIdentityBytes  = 256
	maxModelToolFieldsBytes    = 3 << 20
)

// A generic extension tool may ignore the built-in tools' output caps. The
// result must remain a valid, bounded event even when that tool does not.
// Success content is not silently truncated: the model receives an explicit
// error telling it to save large output in the workspace and return a path.
func boundedToolResult(result modeldata.ToolResult) modeldata.ToolResult {
	if len(result.Content) <= maxDurableToolContentBytes {
		return result
	}
	if result.IsError {
		result.Content = result.Content[:maxDurableToolErrorBytes] + fmt.Sprintf("\n[error detail truncated: %d bytes total]", len(result.Content))
		return result
	}
	result.IsError = true
	result.Code = "TOOL_OUTPUT_TOO_LARGE"
	result.Content = fmt.Sprintf("Tool returned %d bytes, exceeding the %d-byte durable result limit. Save large output to the workspace and return a bounded summary or file path.", len(result.Content), maxDurableToolContentBytes)
	return result
}

func validateModelResponse(response modeldata.AssistantMessage) error {
	if len(response.Content) > maxModelContentBytes {
		return fmt.Errorf("model response content exceeds %d bytes", maxModelContentBytes)
	}
	if len(response.ToolCalls) > maxModelToolCalls {
		return fmt.Errorf("model returned %d tool calls, over limit %d", len(response.ToolCalls), maxModelToolCalls)
	}
	seen := make(map[string]bool, len(response.ToolCalls))
	fields := len(response.Content)
	for _, call := range response.ToolCalls {
		if call.ID == "" || call.Name == "" {
			return fmt.Errorf("model returned tool call without ID or name")
		}
		if len(call.ID) > maxModelToolIdentityBytes || len(call.Name) > maxModelToolIdentityBytes || len(call.Arguments) > maxModelToolArgumentsBytes {
			return fmt.Errorf("model returned oversized tool call identity or arguments")
		}
		fields += len(call.ID) + len(call.Name) + len(call.Arguments)
		if fields > maxModelToolFieldsBytes {
			return fmt.Errorf("model response fields exceed %d bytes", maxModelToolFieldsBytes)
		}
		if seen[call.ID] {
			return fmt.Errorf("model returned duplicate tool call ID %q", call.ID)
		}
		seen[call.ID] = true
	}
	return nil
}
