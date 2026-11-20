package agent

import (
	"context"
	"log/slog"
	"strings"

	"go-bot/harness/output"
	"go-bot/internal/modeldata"
)

// The output policy is selected and any Job limit captured in PreExecute.
// Execution may overlap; this processor runs only in ordered PostExecute.
func prepareToolOutput(ctx context.Context, cwd string, tool Tool, call modeldata.ToolRequest) func(modeldata.ToolResult) modeldata.ToolResult {
	var specialized func(modeldata.ToolResult) modeldata.ToolResult
	if prepared, ok := tool.(PreparedOutputTool); ok {
		specialized = prepared.PrepareOutput([]byte(call.Arguments))
	}
	retained := false
	if managed, ok := tool.(RetainedOutputTool); ok {
		retained = managed.RetainsOutput()
	}
	return func(result modeldata.ToolResult) modeldata.ToolResult {
		if specialized != nil {
			result = specialized(result)
		}
		if retained {
			return boundedToolResult(result)
		}
		var text strings.Builder
		text.WriteString(result.Content)
		hasImage := false
		for _, block := range result.ContentBlocks {
			if block.Type != "text" {
				hasImage = true
				continue
			}
			text.WriteString(block.Text)
		}
		// Upstream preserves mixed content when the model has no image pricing
		// contract. Do not guess an image cost or discard its attachment.
		if hasImage {
			if output.Estimate(text.String()) > output.InlineTokens {
				slog.Warn("tool output retention unavailable: model has no image pricing", "tool", call.Name)
			}
			return boundedToolResult(result)
		}
		preview, err := output.RetainText(context.WithoutCancel(ctx), cwd, ".agent/tool-spill", call.Name, text.String(), output.InlineTokens)
		if err != nil {
			slog.Warn("tool output retention unavailable", "tool", call.Name, "error", err)
		}
		if preview != text.String() {
			result.Content = preview
			result.ContentBlocks = nil
		}
		return boundedToolResult(result)
	}
}
