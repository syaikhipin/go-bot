package agent

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/google/uuid"
)

type RuntimeContextSection struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

func (a *NativeLoop) SetRuntimeContext(provider func() []RuntimeContextSection) {
	a.runtimeContext = provider
}

func (a *NativeLoop) ensureRuntimeContext() error {
	if a.runtimeContext == nil {
		return nil
	}
	sections := a.runtimeContext()
	if len(sections) == 0 {
		return nil
	}
	surface, err := a.session.Surface()
	if err != nil {
		return err
	}
	for _, messageData := range slices.Backward(surface.Messages) {
		// Use retained model messages so a compacted snapshot is reannounced.
		var message struct {
			Source struct {
				Kind     string
				Form     string
				Sections []RuntimeContextSection
			}
		}
		if json.Unmarshal(messageData, &message) != nil || message.Source.Kind != "runtime-context" || message.Source.Form != "snapshot" {
			continue
		}
		if slices.Equal(message.Source.Sections, sections) {
			return nil
		}
		break
	}
	paragraphs := []string{"Current runtime context. This snapshot supersedes earlier runtime-context snapshots."}
	for _, section := range sections {
		paragraphs = append(paragraphs, section.Text)
	}
	message := map[string]any{
		"id": uuid.NewString(), "role": "user",
		"source":  map[string]any{"kind": "runtime-context", "form": "snapshot", "sections": sections},
		"content": []any{map[string]any{"type": "text", "text": strings.Join(paragraphs, "\n\n")}},
	}
	_, err = a.append("user/message", message, json.RawMessage(`"append"`), nil)
	return err
}
