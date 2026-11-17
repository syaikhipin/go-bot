package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"go-bot/harness/agent"
	"go-bot/harness/interaction"
)

type askQuestionArgs struct {
	Timeout   *int `json:"timeout,omitempty"`
	Questions []struct {
		ID          string               `json:"id"`
		Question    string               `json:"question"`
		Header      string               `json:"header,omitempty"`
		Options     []interaction.Option `json:"options,omitempty"`
		MultiSelect bool                 `json:"multi_select,omitempty"`
	} `json:"questions"`
}

func askUserQuestionSpec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "ask_user_question",
		Description: "Ask brief, direct questions about missing information or decisions. Wait up to 120 seconds by default; if no answer arrives, the question remains answerable and you may continue independent work. A pending result is not permission or a skipped answer.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"items":{"type":"object","properties":{"id":{"type":"string"},"question":{"type":"string"},"header":{"type":"string"},"options":{"type":"array","items":{"type":"object","properties":{"label":{"type":"string"},"description":{"type":"string"}},"required":["label"],"additionalProperties":true}},"multi_select":{"type":"boolean"}},"required":["id","question"],"additionalProperties":true}},"timeout":{"type":"integer","description":"Wait seconds for the entire batch (default 120); -1 requires an answer before proceeding."}},"required":["questions"],"additionalProperties":true}`),
	}
}

func decodeAskQuestionArgs(raw json.RawMessage) ([]interaction.Question, int, error) {
	var args askQuestionArgs
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&args); err != nil {
		return nil, 0, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, 0, errors.New("ask_user_question requires one JSON object")
	}
	timeout := 120
	if args.Timeout != nil {
		timeout = *args.Timeout
	}
	if timeout != -1 && (timeout < 1 || timeout > 2_147_483) {
		return nil, 0, errors.New("timeout must be -1 or a positive integer up to 2147483 seconds")
	}
	questions := make([]interaction.Question, 0, len(args.Questions))
	for _, item := range args.Questions {
		questions = append(questions, interaction.Question{
			ID: item.ID, Question: item.Question, Header: item.Header,
			Options: item.Options, MultiSelect: item.MultiSelect,
		})
	}
	return questions, timeout, interaction.ValidateQuestions(questions)
}
