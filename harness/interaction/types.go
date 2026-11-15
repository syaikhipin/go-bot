package interaction

import (
	"errors"
	"fmt"
	"strings"
)

var ErrUnavailable = errors.New("user question answer channel is unavailable")
var ErrNotPending = errors.New("user question is not pending")
var ErrAlreadyAnswered = errors.New("user question already has an answer")
var ErrInvalidAnswer = errors.New("invalid user question answer")
var ErrCancelled = errors.New("user question was cancelled")
var ErrContinued = errors.New("user question is awaiting a late answer")

type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type Question struct {
	ID          string   `json:"id"`
	Question    string   `json:"question"`
	Header      string   `json:"header,omitempty"`
	Options     []Option `json:"options,omitempty"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
	Detail      string   `json:"detail,omitempty"`
}

type AnswerItem struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   *string  `json:"custom,omitempty"`
}

type Answer struct {
	Answers []AnswerItem `json:"answers"`
}

type Approval struct {
	Tool       string `json:"tool"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail"`
	Target     string `json:"target,omitempty"`
	ToolCallID string `json:"toolCallId"`
}

type Pending struct {
	Approval  *Approval  `json:"approval,omitempty"`
	CallID    string     `json:"callId"`
	Questions []Question `json:"questions"`
	Turn      uint64     `json:"turn"`
	Step      uint64     `json:"step"`
	State     string     `json:"state,omitempty"`
}

type settled struct {
	answer Answer
	err    error
}

func ValidateQuestions(questions []Question) error {
	if len(questions) == 0 {
		return errors.New("ask_user_question requires at least one question")
	}
	seen := make(map[string]bool, len(questions))
	for _, question := range questions {
		if question.ID == "" || strings.TrimSpace(question.ID) != question.ID || strings.TrimSpace(question.Question) == "" || seen[question.ID] {
			return errors.New("question ids must be distinct and prompts nonempty")
		}
		seen[question.ID] = true
		labels := make(map[string]bool, len(question.Options))
		for _, option := range question.Options {
			if strings.TrimSpace(option.Label) == "" || labels[option.Label] {
				return fmt.Errorf("question %s has empty or duplicate option labels", question.ID)
			}
			labels[option.Label] = true
		}
	}
	return nil
}

func validateAnswer(questions []Question, answer Answer) error {
	if len(answer.Answers) != len(questions) {
		return ErrInvalidAnswer
	}
	byID := make(map[string]Question, len(questions))
	for _, question := range questions {
		byID[question.ID] = question
	}
	seen := make(map[string]bool, len(answer.Answers))
	for _, item := range answer.Answers {
		question, ok := byID[item.ID]
		if !ok || seen[item.ID] || (!question.MultiSelect && len(item.Selected) > 1) {
			return ErrInvalidAnswer
		}
		seen[item.ID] = true
		choices := make(map[string]bool, len(question.Options))
		for _, option := range question.Options {
			choices[option.Label] = true
		}
		used := make(map[string]bool, len(item.Selected))
		for _, label := range item.Selected {
			if !choices[label] || used[label] {
				return ErrInvalidAnswer
			}
			used[label] = true
		}
	}
	return nil
}

// ValidateAnswer applies the same batch check to a late answer recovered from
// the v4 log as to an answer delivered during the foreground wait.
func ValidateAnswer(questions []Question, answer Answer) error {
	return validateAnswer(questions, answer)
}

func normalizeAnswer(answer Answer) Answer {
	copy := Answer{Answers: make([]AnswerItem, len(answer.Answers))}
	for index, item := range answer.Answers {
		copy.Answers[index] = AnswerItem{ID: item.ID, Selected: append([]string{}, item.Selected...)}
		if item.Custom != nil {
			custom := *item.Custom
			copy.Answers[index].Custom = &custom
		}
	}
	return copy
}
