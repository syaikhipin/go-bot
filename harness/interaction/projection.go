package interaction

import (
	"encoding/json"

	"go-harness/harness/session"
)

// FoldTimedQuestions derives answerable questions solely from the v4 log.
// The tool schema in each request/header distinguishes timed calls from older
// blocking calls, as in the upstream userQuestions projection.
func FoldTimedQuestions(events []sessionformat.Event) []Pending {
	active := make(map[string]Pending)
	order := make([]string, 0)
	timed := false
	for _, event := range events {
		switch event.Type {
		case "request/header":
			var row struct {
				Header struct {
					Tools []struct {
						Name       string                     `json:"name"`
						Parameters map[string]json.RawMessage `json:"parameters"`
					} `json:"tools"`
				} `json:"header"`
			}
			if json.Unmarshal(event.Data, &row) != nil {
				timed = false
				continue
			}
			timed = false
			for _, tool := range row.Header.Tools {
				if tool.Name != "ask_user_question" {
					continue
				}
				var properties map[string]json.RawMessage
				if json.Unmarshal(tool.Parameters["properties"], &properties) == nil {
					_, timed = properties["timeout"]
				}
			}
		case "tool/call":
			if !timed {
				continue
			}
			var row struct {
				Turn      uint64 `json:"turn"`
				Step      uint64 `json:"step"`
				CallID    string `json:"callId"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if json.Unmarshal(event.Data, &row) != nil || row.Name != "ask_user_question" || row.CallID == "" {
				continue
			}
			var args struct {
				Questions []struct {
					ID          string   `json:"id"`
					Question    string   `json:"question"`
					Header      string   `json:"header"`
					Options     []Option `json:"options"`
					MultiSelect bool     `json:"multi_select"`
				} `json:"questions"`
			}
			if json.Unmarshal([]byte(row.Arguments), &args) != nil || len(args.Questions) == 0 {
				continue
			}
			questions := make([]Question, 0, len(args.Questions))
			for _, item := range args.Questions {
				questions = append(questions, Question{ID: item.ID, Question: item.Question, Header: item.Header,
					Options: item.Options, MultiSelect: item.MultiSelect})
			}
			if ValidateQuestions(questions) != nil {
				continue
			}
			for index, id := range order {
				if id == row.CallID {
					order = append(order[:index], order[index+1:]...)
					break
				}
			}
			order = append(order, row.CallID)
			active[row.CallID] = Pending{CallID: row.CallID, Questions: questions, Turn: row.Turn, Step: row.Step, State: "open"}
		case "tool/result":
			var row struct {
				Message struct {
					ToolCallID string `json:"toolCallId"`
					IsError    bool   `json:"isError"`
					Content    []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(event.Data, &row) != nil {
				continue
			}
			pending, ok := active[row.Message.ToolCallID]
			if !ok {
				continue
			}
			if row.Error.Code == "TOOL_OUTCOME_UNKNOWN" {
				pending.State = "continued"
				active[pending.CallID] = pending
				continue
			}
			isPending := false
			if !row.Message.IsError && len(row.Message.Content) > 0 {
				var payload struct {
					Pending bool `json:"pending"`
				}
				isPending = json.Unmarshal([]byte(row.Message.Content[0].Text), &payload) == nil && payload.Pending
			}
			if isPending {
				pending.State = "continued"
				active[pending.CallID] = pending
			} else {
				delete(active, pending.CallID)
			}
		case "user/message":
			var row struct {
				Source struct {
					Kind   string `json:"kind"`
					CallID string `json:"callId"`
				} `json:"source"`
			}
			if json.Unmarshal(event.Data, &row) == nil && row.Source.Kind == "user-question-reply" {
				delete(active, row.Source.CallID)
			}
		}
	}
	result := make([]Pending, 0, len(active))
	for _, id := range order {
		if pending, ok := active[id]; ok {
			result = append(result, pending)
		}
	}
	return result
}
