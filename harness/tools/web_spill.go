package tools

import (
	"context"
	"go-bot/harness/output"
	"log/slog"
)

const webInlineTokens = output.InlineTokens

func webTextTokens(text string) int { return output.Estimate(text) }
func retainWebText(ctx context.Context, workdir, name, text string) string {
	retained, err := output.RetainText(ctx, workdir, ".agent/web-spill", name, text, webInlineTokens)
	if err != nil {
		slog.Warn("tool output retention unavailable", "tool", name, "error", err)
	}
	return retained
}
