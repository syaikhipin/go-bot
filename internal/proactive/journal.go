package proactive

import (
	"encoding/json"
	"time"

	sessionformat "go-bot/harness/session"
)

// CommittedResults folds existing v4 facts only. An assistant's advertised
// arguments, an unsuccessful tool/result, or an interrupted turn is not a
// completed proactive decision. No extra result or notification log is needed.
func CommittedResults(events []sessionformat.Event) []Completed {
	var turn uint64
	var candidate *Result
	intents := make(map[string]Result)
	var results []Completed
	for _, event := range events {
		switch event.Type {
		case "turn/start":
			var data struct{ Turn uint64 }
			if json.Unmarshal(event.Data, &data) == nil {
				turn = data.Turn
			}
			candidate = nil
			clear(intents)
		case "tool/call":
			var data struct {
				Turn      uint64 `json:"turn"`
				CallID    string `json:"callId"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if json.Unmarshal(event.Data, &data) != nil || turn == 0 || data.Turn != turn || data.Name != "structured_output" {
				continue
			}
			if result, err := DecodeResult(json.RawMessage(data.Arguments), time.UnixMilli(event.Time)); err == nil {
				intents[data.CallID] = result
			}
		case "tool/result":
			var data struct {
				Turn    uint64 `json:"turn"`
				Message struct {
					CallID  string `json:"toolCallId"`
					IsError bool   `json:"isError"`
				} `json:"message"`
			}
			if json.Unmarshal(event.Data, &data) != nil || data.Turn != turn || turn == 0 {
				continue
			}
			if result, ok := intents[data.Message.CallID]; ok && !data.Message.IsError {
				copy := result
				candidate = &copy
			}
			delete(intents, data.Message.CallID)
		case "turn/end":
			var data struct {
				Turn   uint64                `json:"turn"`
				Reason struct{ Kind string } `json:"reason"`
			}
			if json.Unmarshal(event.Data, &data) == nil && data.Turn == turn && turn != 0 && data.Reason.Kind == "completed" && candidate != nil {
				results = append(results, Completed{Turn: turn, CompletedAt: event.Time, Result: *candidate})
			}
			turn, candidate = 0, nil
			clear(intents)
		}
	}
	return results
}
