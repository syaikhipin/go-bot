package tools

import (
	"encoding/json"
	"testing"

	"go-harness/harness/interaction"
)

func TestAskUserQuestionAcceptsPresentationExtensions(t *testing.T) {
	const raw = `{"questions":[{"id":"choice","question":"Which path?","header":"Choose","options":[{"label":"Fast","description":"Finish sooner","icon":"bolt"}],"multi_select":false,"layout":"compact"}],"client_hint":"card"}`
	if err := (NativeAskUserQuestion{Questions: interaction.NewNativeQuestions()}).Validate(json.RawMessage(raw)); err != nil {
		t.Fatalf("upstream-open ask_user_question arguments were rejected: %v", err)
	}
	questions, _, err := decodeAskQuestionArgs(json.RawMessage(raw))
	if err != nil || len(questions) != 1 || questions[0].ID != "choice" || questions[0].Question != "Which path?" ||
		len(questions[0].Options) != 1 || questions[0].Options[0].Label != "Fast" {
		t.Fatalf("decoded questions = %+v, %v", questions, err)
	}
	var schema struct {
		AdditionalProperties bool `json:"additionalProperties"`
		Properties           map[string]struct {
			Items struct {
				AdditionalProperties bool `json:"additionalProperties"`
				Properties           map[string]struct {
					Items struct {
						AdditionalProperties bool `json:"additionalProperties"`
					} `json:"items"`
				} `json:"properties"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(askUserQuestionSpec().Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	question := schema.Properties["questions"].Items
	if !schema.AdditionalProperties || !question.AdditionalProperties || !question.Properties["options"].Items.AdditionalProperties {
		t.Fatal("ask_user_question schema must preserve the upstream open parameter and nested presentation fields")
	}
}

func TestAskUserQuestionTimedDefaultsAndExplicitWait(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want int
		bad  bool
	}{
		{"default", `{"questions":[{"id":"q","question":"Answer?"}]}`, 120, false},
		{"indefinite", `{"questions":[{"id":"q","question":"Answer?"}],"timeout":-1}`, -1, false},
		{"short", `{"questions":[{"id":"q","question":"Answer?"}],"timeout":2}`, 2, false},
		{"zero", `{"questions":[{"id":"q","question":"Answer?"}],"timeout":0}`, 0, true},
		{"fractional", `{"questions":[{"id":"q","question":"Answer?"}],"timeout":1.5}`, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, timeout, err := decodeAskQuestionArgs(json.RawMessage(test.raw))
			if (err != nil) != test.bad || (!test.bad && timeout != test.want) {
				t.Fatalf("timeout = %d, error = %v", timeout, err)
			}
		})
	}
}
