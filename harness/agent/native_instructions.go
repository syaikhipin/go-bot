package agent

import (
	"context"
	"encoding/json"
	"strings"

	"go-bot/internal/instructionrender"

	"github.com/google/uuid"
)

// ensureInstructions records changed project guidance after the directly
// claimed message and before the model request. Re-reading on each step also
// notices files changed by Shell, not just by the dedicated file tools.
func (a *NativeLoop) ensureInstructions(ctx context.Context) error {
	if a.instructionsForStep == nil {
		return nil
	}
	if err := a.scanInstructionFileTouches(); err != nil {
		return err
	}
	snapshot, err := a.instructionsForStep(ctx, a.instructionTouched)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	visible, err := a.visibleInstructions()
	if err != nil {
		return err
	}
	if !visible.baseline && snapshot.Text == "" && len(visible.active) == 0 {
		return nil
	}
	baseline := !visible.baseline || visible.identity != snapshot.Identity
	if baseline && visible.baseline && len(snapshot.UnavailableGroups) > 0 {
		// A root/configuration change cannot replace the old baseline while
		// some new candidate groups are unobservable. Retry on the next step.
		return nil
	}
	var messageText string
	var changes []NativeInstructionChange
	if baseline {
		included := snapshot.Changes
		messageText = snapshot.Text
		if visible.baseline || len(visible.active) > 0 {
			if snapshot.ReplacementText != "" {
				messageText = snapshot.ReplacementText
				included = snapshot.ReplacementChanges
			} else {
				messageText, included = renderNativeFallbackReplacement(snapshot)
			}
		}
		changes = nativeInstructionBaselineChanges(visible.active, included, snapshot.UnavailableGroups)
	} else {
		changes = nativeInstructionDelta(visible.active, snapshot)
		if len(changes) == 0 {
			return nil
		}
		messageText, changes = renderNativeInstructionDelta(changes, snapshot.Content)
		if len(changes) == 0 {
			return nil
		}
	}
	message := map[string]any{
		"id": uuid.NewString(), "role": "user",
		"source": map[string]any{
			"kind": "agent-instructions", "form": "instructions", "changes": changes,
		},
		"content": []any{map[string]any{"type": "text", "text": messageText}},
	}
	if baseline {
		source := message["source"].(map[string]any)
		source["baseline"] = true
		source["baselineIdentity"] = snapshot.Identity
	}
	_, err = a.append("user/message", message, json.RawMessage(`"append"`), nil)
	return err
}

// A custom provider can supply only the baseline shape. Keep its replacement
// bounded too; the production file provider supplies the exact separately
// rendered replacement (including sources omitted by the normal baseline).
func renderNativeFallbackReplacement(snapshot NativeInstructionSnapshot) (string, []NativeInstructionChange) {
	files := make([]instructionrender.File, len(snapshot.Changes))
	for index, change := range snapshot.Changes {
		files[index] = instructionrender.File{Path: change.Path, Content: snapshot.Content[change.Scope]}
	}
	intro := instructionrender.ReplacementIntro
	if len(files) == 0 {
		intro = instructionrender.EmptyReplacementIntro
	}
	rendered := instructionrender.Render(files, nativeInstructionDeltaMaxBytes, intro, func(file instructionrender.File) string {
		return "Instructions from: " + file.Path + "\n\n" + file.Content
	})
	included := make([]NativeInstructionChange, len(rendered.Represented))
	for index, original := range rendered.Represented {
		included[index] = snapshot.Changes[original]
	}
	return rendered.Text, included
}

// A successful file-tool result makes nested instructions along that touched
// path relevant. Reconstruct this from durable calls/results on cold resume;
// no process-local touch cache is required for correctness.
func (a *NativeLoop) scanInstructionFileTouches() error {
	events, err := a.session.DurableEventsFrom(a.instructionScanned)
	if err != nil {
		return err
	}
	if a.instructionCalls == nil {
		a.instructionCalls = make(map[string]string)
	}
	seen := make(map[string]bool, len(a.instructionTouched))
	for _, path := range a.instructionTouched {
		seen[path] = true
	}
	for _, event := range events {
		switch event.Type {
		case "tool/call":
			var call struct {
				CallID    string `json:"callId"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if json.Unmarshal(event.Data, &call) != nil ||
				(call.Name != "readfile" && call.Name != "writefile" && call.Name != "editfile") {
				continue
			}
			var args struct {
				Path string `json:"path"`
			}
			if json.Unmarshal([]byte(call.Arguments), &args) == nil && strings.TrimSpace(args.Path) != "" {
				a.instructionCalls[call.CallID] = args.Path
			}
		case "tool/result":
			var result struct {
				Message struct {
					ToolCallID string `json:"toolCallId"`
					IsError    bool   `json:"isError"`
				} `json:"message"`
			}
			if json.Unmarshal(event.Data, &result) != nil {
				continue
			}
			path := a.instructionCalls[result.Message.ToolCallID]
			delete(a.instructionCalls, result.Message.ToolCallID)
			if path != "" && !result.Message.IsError && !seen[path] {
				seen[path] = true
				a.instructionTouched = append(a.instructionTouched, path)
			}
		}
	}
	a.instructionScanned += len(events)
	return nil
}
