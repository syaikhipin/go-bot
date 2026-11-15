package session

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInboxProjectionUsesOnlyDurableTailAndReplaysOnResume(t *testing.T) {
	header := appendStoreHeader()
	s, err := New(header, activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	first := InboxRecord{MessageID: "m1", TurnID: "t1", IdempotencyKey: "k1", Content: "first"}
	second := InboxRecord{MessageID: "m2", TurnID: "t2", IdempotencyKey: "k2", Content: "second"}
	for _, record := range []InboxRecord{first, second} {
		if _, err := s.Append("user/inbox", 0, 0, record); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, found, err := s.NextInbox(); err != nil || found || s.inbox.processed != 0 {
		t.Fatalf("unflushed input projected: found=%t processed=%d err=%v", found, s.inbox.processed, err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, turn, found, err := s.NextInbox()
	if err != nil || !found || got != first || turn != 1 || s.inbox.processed != 2 {
		t.Fatalf("first projected input = %#v turn=%d found=%t processed=%d err=%v", got, turn, found, s.inbox.processed, err)
	}
	if _, err := s.Append("turn/cancel", 0, 0, map[string]string{"messageId": "m1", "turnId": "t1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, turn, found, err = s.NextInbox()
	if err != nil || !found || got != second || turn != 1 || s.inbox.processed != 3 {
		t.Fatalf("cancelled head was not skipped = %#v turn=%d found=%t processed=%d err=%v", got, turn, found, s.inbox.processed, err)
	}
	if _, err := s.Append("turn/start", 1, 0, map[string]string{"messageId": "m2", "turnId": "t2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("user/message", 1, 0, map[string]string{"content": "second"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("turn/end", 1, 0, map[string]string{"reason": "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, found, err := s.InboxByTurn("t2")
	if err != nil || !found || !state.Finished || state.StartedTurn != 1 {
		t.Fatalf("finished turn = %#v found=%t err=%v", state, found, err)
	}
	if _, next, found, err := s.NextInbox(); err != nil || found || next != 2 {
		t.Fatalf("drained inbox = next=%d found=%t err=%v", next, found, err)
	}
	if state, found, err := s.InboxByKey("k1"); err != nil || !found || !state.Cancelled || state.Record != first {
		t.Fatalf("cancelled idempotency record = %#v found=%t err=%v", state, found, err)
	}
	if s.inbox.processed != len(s.DurableEvents()) {
		t.Fatalf("projection did not catch up: %d", s.inbox.processed)
	}
	resumed, err := Resume(header, s.DurableEvents(), activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	if state, found, err := resumed.InboxByKey("k2"); err != nil || !found || !state.Finished || resumed.inbox.processed != len(s.DurableEvents()) {
		t.Fatalf("cold replay = %#v found=%t processed=%d err=%v", state, found, resumed.inbox.processed, err)
	}
}

func TestInboxCannotStartAnotherTurnUntilOpenTailIsRepaired(t *testing.T) {
	s, err := New(appendStoreHeader(), activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []InboxRecord{
		{MessageID: "m1", TurnID: "t1", IdempotencyKey: "k1", Content: "first"},
		{MessageID: "m2", TurnID: "t2", IdempotencyKey: "k2", Content: "second"},
	} {
		if _, err := s.Append("user/inbox", 0, 0, record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append("turn/start", 1, 0, map[string]string{"messageId": "m1", "turnId": "t1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("user/message", 1, 0, map[string]string{"role": "user", "content": "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("step/start", 1, 1, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.NextInbox(); !errors.Is(err, ErrOpenTurnNeedsRecovery) {
		t.Fatalf("another queued turn could start over open turn: %v", err)
	}
	if _, err := s.NextTurn(); !errors.Is(err, ErrOpenTurnNeedsRecovery) {
		t.Fatalf("direct turn could start over open turn: %v", err)
	}
	if repaired, err := RepairInterrupted(context.Background(), s); err != nil || !repaired {
		t.Fatalf("repair open tail = %t, %v", repaired, err)
	}
	if next, turn, found, err := s.NextInbox(); err != nil || !found || next.MessageID != "m2" || turn != 2 {
		t.Fatalf("queued turn after repair = %#v turn=%d found=%t err=%v", next, turn, found, err)
	}
}

func TestInboxProjectionRejectsDuplicateIdentityAndInvalidTurnSource(t *testing.T) {
	for name, entries := range map[string][]struct {
		kind string
		turn uint64
		data any
	}{
		"duplicate-key": {
			{"user/inbox", 0, InboxRecord{MessageID: "m1", TurnID: "t1", IdempotencyKey: "key", Content: "first"}},
			{"user/inbox", 0, InboxRecord{MessageID: "m2", TurnID: "t2", IdempotencyKey: "key", Content: "second"}},
		},
		"unknown-source": {
			{"turn/start", 1, map[string]string{"messageId": "missing", "turnId": "missing"}},
		},
		"out-of-order": {
			{"user/inbox", 0, InboxRecord{MessageID: "m1", TurnID: "t1", IdempotencyKey: "k1", Content: "first"}},
			{"user/inbox", 0, InboxRecord{MessageID: "m2", TurnID: "t2", IdempotencyKey: "k2", Content: "second"}},
			{"turn/start", 1, map[string]string{"messageId": "m2", "turnId": "t2"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := New(appendStoreHeader(), activityJournal{})
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if _, err := s.Append(entry.kind, entry.turn, 0, entry.data); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := s.NextInbox(); err == nil || !strings.Contains(err.Error(), map[string]string{"duplicate-key": "duplicate", "unknown-source": "source", "out-of-order": "source"}[name]) {
				t.Fatalf("invalid durable inbox accepted: %v", err)
			}
		})
	}
}

func TestInternalJobInboxHasIndependentIdentityAndReplays(t *testing.T) {
	header := appendStoreHeader()
	s, err := New(header, activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	human := InboxRecord{MessageID: "human-1", TurnID: "turn-1", IdempotencyKey: "job:shared", Content: "work"}
	job := InboxRecord{MessageID: "job-1", TurnID: "turn-2", IdempotencyKey: "job:shared", Content: "job done", SourceKind: "job", SourceID: "shared"}
	if _, err := s.Append("user/inbox", 0, 0, human); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("agent/inbox", 0, 0, job); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateInbox(); err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.InboxByKey("job:shared"); err != nil || !found || got.Record != human {
		t.Fatalf("human key lookup = %#v found=%t err=%v", got, found, err)
	}
	if got, found, err := s.InboxByInternalSource("job", "shared"); err != nil || !found || got.Record != job {
		t.Fatalf("Job source lookup = %#v found=%t err=%v", got, found, err)
	}
	if err := ValidateExecutionTrace(s.DurableEvents()); err != nil {
		t.Fatal(err)
	}
	resumed, err := Resume(header, s.DurableEvents(), activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	if got, found, err := resumed.InboxByInternalSource("job", "shared"); err != nil || !found || got.Record != job {
		t.Fatalf("replayed Job source = %#v found=%t err=%v", got, found, err)
	}
	if activity, err := resumed.DurableActivity(); err != nil || activity.Idle {
		t.Fatalf("replayed internal inbox activity = %#v, %v", activity, err)
	}
}

func TestColdResumeWithdrawsQueuedGoalWithoutCancellingHumanInput(t *testing.T) {
	ctx := context.Background()
	header := appendStoreHeader()
	s, err := New(header, activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	goal := InboxRecord{
		MessageID: "goal-message", TurnID: "goal-turn", IdempotencyKey: "goal:goal-one:1:1",
		Content: "continue", SourceKind: "goal", SourceID: "goal-one:1:1",
		GoalID: "goal-one", GoalRevision: 1, GoalRound: 1,
	}
	human := InboxRecord{MessageID: "human-message", TurnID: "human-turn", IdempotencyKey: "human", Content: "new request"}
	if _, err := s.Append("goal/change", 0, 0, GoalChange{
		Kind: "goal/change", Version: GoalChangeVersion, Operation: "create",
		Goal:      &GoalSnapshot{ID: "goal-one", Revision: 1, Objective: "finish", Phase: "active", MaxGoalRounds: 3},
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("agent/inbox", 0, 0, goal); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("user/inbox", 0, 0, human); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err := Resume(header, s.DurableEvents(), activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	withdrawn, err := restored.WithdrawQueuedGoalRounds(ctx)
	if err != nil || withdrawn != 1 {
		t.Fatalf("withdrawn=%d err=%v", withdrawn, err)
	}
	if again, err := restored.WithdrawQueuedGoalRounds(ctx); err != nil || again != 0 {
		t.Fatalf("repeated withdrawal=%d err=%v", again, err)
	}
	if next, _, found, err := restored.NextInbox(); err != nil || !found || next.MessageID != human.MessageID {
		t.Fatalf("next input=%#v found=%t err=%v", next, found, err)
	}
	if current, found, err := restored.CurrentGoal(); err != nil || !found || current.RoundsStarted != 0 {
		t.Fatalf("withdrawal counted a goal round: %#v found=%t err=%v", current, found, err)
	}
	if err := ValidateExecutionTrace(restored.DurableEvents()); err != nil {
		t.Fatalf("withdrawal trace = %v", err)
	}
}
