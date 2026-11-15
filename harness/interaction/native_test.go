package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestNativeQuestionsAnswerAndCancelWithoutSessionEvents(t *testing.T) {
	questions := NewNativeQuestions()
	questions.Enable()
	request := []Question{{ID: "choice", Question: "Which option?", Options: []Option{{Label: "A"}, {Label: "B"}}}}
	answered := make(chan error, 1)
	go func() {
		answer, err := questions.Ask(context.Background(), 1, 1, "call-1", request)
		if err == nil && (len(answer.Answers) != 1 || answer.Answers[0].Selected[0] != "B") {
			err = ErrInvalidAnswer
		}
		answered <- err
	}()
	deadline := time.After(2 * time.Second)
	for len(questions.Pending()) != 1 {
		select {
		case <-deadline:
			t.Fatal("native question did not become pending")
		case <-time.After(time.Millisecond):
		}
	}
	if err := questions.Answer(context.Background(), "call-1", Answer{Answers: []AnswerItem{{ID: "wrong", Selected: []string{"A"}}}}); !errors.Is(err, ErrInvalidAnswer) {
		t.Fatalf("invalid native answer = %v", err)
	}
	if err := questions.Answer(context.Background(), "call-1", Answer{Answers: []AnswerItem{{ID: "choice", Selected: []string{"B"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := <-answered; err != nil {
		t.Fatal(err)
	}
	cancelled := make(chan error, 1)
	go func() {
		_, err := questions.Ask(context.Background(), 2, 1, "call-2", request)
		cancelled <- err
	}()
	deadline = time.After(2 * time.Second)
	for len(questions.Pending()) != 1 {
		select {
		case <-deadline:
			t.Fatal("second native question did not become pending")
		case <-time.After(time.Millisecond):
		}
	}
	if err := questions.Cancel(context.Background(), "call-2"); err != nil {
		t.Fatal(err)
	}
	if err := <-cancelled; !errors.Is(err, ErrCancelled) {
		t.Fatalf("native cancellation = %v", err)
	}
	questions.Close()
}

func TestNativeQuestionsSkippedAnswerEndsWait(t *testing.T) {
	questions := NewNativeQuestions()
	questions.Enable()
	defer questions.Close()
	result := make(chan struct {
		answer Answer
		err    error
	}, 1)
	go func() {
		answer, err := questions.Ask(context.Background(), 1, 1, "skip-call", []Question{{
			ID: "choice", Question: "Choose or skip?", Options: []Option{{Label: "A"}},
		}})
		result <- struct {
			answer Answer
			err    error
		}{answer, err}
	}()
	deadline := time.After(2 * time.Second)
	for len(questions.Pending()) != 1 {
		select {
		case <-deadline:
			t.Fatal("native question did not become pending")
		case <-time.After(time.Millisecond):
		}
	}
	if err := questions.Answer(context.Background(), "skip-call", Answer{Answers: []AnswerItem{{ID: "choice", Selected: []string{}}}}); err != nil {
		t.Fatalf("skip answer was rejected: %v", err)
	}
	select {
	case settled := <-result:
		if settled.err != nil {
			t.Fatal(settled.err)
		}
		encoded, err := json.Marshal(normalizeAnswer(settled.answer))
		if err != nil || string(encoded) != `{"answers":[{"id":"choice","selected":[]}]}` {
			t.Fatalf("skip result = %s, %v", encoded, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("skipped answer did not release the waiting tool")
	}
}

func TestNativeQuestionsRejectsDuplicateAnswerIDs(t *testing.T) {
	questions := NewNativeQuestions()
	questions.Enable()
	request := []Question{
		{ID: "first", Question: "First?", Options: []Option{{Label: "A"}}},
		{ID: "second", Question: "Second?", Options: []Option{{Label: "B"}}},
	}
	result := make(chan error, 1)
	go func() {
		_, err := questions.Ask(context.Background(), 1, 1, "duplicate-call", request)
		result <- err
	}()
	deadline := time.After(2 * time.Second)
	for len(questions.Pending()) != 1 {
		select {
		case <-deadline:
			t.Fatal("native question did not become pending")
		case <-time.After(time.Millisecond):
		}
	}
	duplicate := Answer{Answers: []AnswerItem{
		{ID: "first", Selected: []string{"A"}},
		{ID: "first", Selected: []string{"A"}},
	}}
	if err := questions.Answer(context.Background(), "duplicate-call", duplicate); !errors.Is(err, ErrInvalidAnswer) {
		t.Fatalf("duplicate question answer IDs accepted: %v", err)
	}
	if err := questions.Cancel(context.Background(), "duplicate-call"); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancelled pending duplicate test: %v", err)
	}
}

func TestNativeQuestionsTimedWaitLeavesContinuedQuestion(t *testing.T) {
	questions := NewNativeQuestions()
	questions.Enable()
	defer questions.Close()
	batch := []Question{{ID: "choice", Question: "Choose?", Options: []Option{{Label: "A"}}}}
	_, pending, err := questions.AskTimed(context.Background(), 1, 1, "timed-call", batch, 10*time.Millisecond)
	if err != nil || !pending {
		t.Fatalf("timed wait = pending %t, %v", pending, err)
	}
	if view := questions.Pending(); len(view) != 1 || view[0].State != "continued" {
		t.Fatalf("continued question = %+v", view)
	}
	if err := questions.Answer(context.Background(), "timed-call", Answer{Answers: []AnswerItem{{ID: "choice", Selected: []string{"A"}}}}); !errors.Is(err, ErrContinued) {
		t.Fatalf("late answer incorrectly settled the old tool call: %v", err)
	}
}
