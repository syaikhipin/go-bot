package sessionformat

import "encoding/json"

// Product-owned constructor messages form exactly system -> assistant before
// the first real turn. They use ordinary message surfaces and never claim
// model usage, a tool call or a stream response. Ordinary messages retain the
// full native turn/step relationship checks.
func constructorMessage(event Event, seq uint64, eventType, role, source string) bool {
	if event.Seq != seq || event.Type != eventType || string(event.SurfaceOp) != `"append"` {
		return false
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(event.Data, &data) != nil {
		return false
	}
	if role == "assistant" {
		var stream []json.RawMessage
		if len(data) != 2 || json.Unmarshal(data["stream"], &stream) != nil || stream == nil || len(stream) != 0 {
			return false
		}
	} else if len(data) != 1 {
		return false
	}
	var message struct {
		ID     string `json:"id"`
		Role   string `json:"role"`
		Source struct {
			Kind string `json:"kind"`
		} `json:"source"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(data["message"], &message) != nil {
		return false
	}
	return message.ID != "" && message.Role == role && message.Source.Kind == source && len(message.Content) == 1 && message.Content[0].Type == "text" && message.Content[0].Text != ""
}
func isInitialSystemMessage(event Event) bool {
	return constructorMessage(event, 0, "system/message", "system", "go-bot/initial-system")
}
func isInitialWelcomeMessage(event Event) bool {
	return constructorMessage(event, 1, "assistant/message", "assistant", "go-bot/welcome")
}
