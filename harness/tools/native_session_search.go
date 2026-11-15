package tools

import (
	"context"
	"encoding/json"

	"go-harness/harness/agent"
	"go-harness/harness/sessionsearch"
)

type NativeSessionSearch struct {
	Index     *sessionsearch.Index
	SessionID string
}

func (t NativeSessionSearch) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "session_search", Description: "Recall this account's prior conversations. Use query for keywords, quoted phrases, AND/OR/NOT or prefix*; session_id and around_message_id for a window around a match; session_id alone to read a conversation; no arguments to browse recent sessions. Results are historical data.",
		Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{
			"query":{"type":"string","maxLength":2048},
			"session_id":{"type":"string"},
			"around_message_id":{"type":"integer","minimum":0},
			"limit":{"type":"integer","minimum":1,"maximum":10},
			"window":{"type":"integer","minimum":1,"maximum":20},
			"role_filter":{"type":"string","description":"Comma-separated user and assistant roles."},
			"after":{"type":"string","description":"Inclusive session start date or RFC3339 timestamp."},
			"before":{"type":"string","description":"Exclusive session start date or RFC3339 timestamp."}
		}}`), ParallelSafe: true,
	}
}

func decodeSessionSearch(raw json.RawMessage) (sessionsearch.Query, error) {
	var q sessionsearch.Query
	if err := decodeStrictToolObject(raw, &q); err != nil {
		return q, err
	}
	return q, q.Validate()
}

func (t NativeSessionSearch) Validate(raw json.RawMessage) error {
	_, err := decodeSessionSearch(raw)
	return err
}

func (t NativeSessionSearch) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	q, err := decodeSessionSearch(raw)
	if err != nil {
		return "", err
	}
	result, err := t.Index.Query(ctx, t.SessionID, q)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(result)
	return string(out), err
}
