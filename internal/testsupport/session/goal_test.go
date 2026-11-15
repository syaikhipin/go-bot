package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestGoalProjectionCountsOnlyAdmittedRoundsAndColdReplays(t *testing.T) {
	header := appendStoreHeader()
	s, err := New(header, activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	created := GoalChange{Kind: "goal/change", Version: GoalChangeVersion, Operation: "create", Goal: &GoalSnapshot{ID: "goal-one", Revision: 1, Objective: "finish report", Phase: "active", MaxGoalRounds: 3}, RoundsStarted: 0, CreatedAt: 100, UpdatedAt: 100}
	if _, err := s.Append("goal/change", 1, 1, created); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.CurrentGoal(); err != nil || found {
		t.Fatalf("unflushed goal leaked: found=%t err=%v", found, err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, found, err := s.CurrentGoal(); err != nil || !found || state.Goal.Revision != 1 || state.RoundsStarted != 0 {
		t.Fatalf("created goal = %#v found=%t err=%v", state, found, err)
	}
	if _, err := s.Append("user/message", 2, 0, map[string]any{"role": "user", "content": "continue", "source": map[string]any{"kind": "goal", "goalId": "goal-one", "revision": 1, "round": 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, found, err := s.CurrentGoal(); err != nil || !found || state.RoundsStarted != 1 {
		t.Fatalf("admitted round = %#v found=%t err=%v", state, found, err)
	}
	resumed, err := Resume(header, s.DurableEvents(), activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.ValidateGoal(); err != nil {
		t.Fatal(err)
	}
	if state, found, err := resumed.CurrentGoal(); err != nil || !found || state.RoundsStarted != 1 || state.Goal.Objective != "finish report" {
		t.Fatalf("cold replay = %#v found=%t err=%v", state, found, err)
	}
	// Returned views are detached from the projection cache.
	state, _, _ := resumed.CurrentGoal()
	state.Goal.Objective = "tampered"
	if again, _, _ := resumed.CurrentGoal(); again.Goal.Objective != "finish report" {
		t.Fatalf("goal view aliased live projection: %#v", again)
	}
}

func TestGoalProjectionRejectsStaleRevisionAndInvalidRound(t *testing.T) {
	for _, trial := range []struct {
		name      string
		eventType string
		data      any
		want      string
	}{
		{"stale revision", "goal/change", GoalChange{Kind: "goal/change", Version: GoalChangeVersion, Operation: "edit", Goal: &GoalSnapshot{ID: "goal-one", Revision: 3, Objective: "changed", Phase: "active", MaxGoalRounds: 3}, RoundsStarted: 0, CreatedAt: 100, UpdatedAt: 101}, "revision"},
		{"skipped round", "user/message", map[string]any{"role": "user", "content": "continue", "source": map[string]any{"kind": "goal", "goalId": "goal-one", "revision": 1, "round": 2}}, "next admitted round"},
		{"malformed blocker", "goal/change", GoalChange{Kind: "goal/change", Version: GoalChangeVersion, Operation: "block", Goal: &GoalSnapshot{ID: "goal-one", Revision: 2, Objective: "finish", Phase: "blocked", MaxGoalRounds: 3, BlockedReason: &GoalBlockReason{Code: "Bad_Code", Message: "oops"}}, RoundsStarted: 0, CreatedAt: 100, UpdatedAt: 101}, "blocker"},
		{"unexpected field", "goal/change", json.RawMessage(`{"kind":"goal/change","version":1,"operation":"edit","goal":{"id":"goal-one","revision":2,"objective":"finish","phase":"active","maxGoalRounds":3},"roundsStarted":0,"createdAt":100,"updatedAt":101,"extra":true}`), "unknown field"},
	} {
		t.Run(trial.name, func(t *testing.T) {
			s, err := New(appendStoreHeader(), activityJournal{})
			if err != nil {
				t.Fatal(err)
			}
			created := GoalChange{Kind: "goal/change", Version: GoalChangeVersion, Operation: "create", Goal: &GoalSnapshot{ID: "goal-one", Revision: 1, Objective: "finish", Phase: "active", MaxGoalRounds: 3}, RoundsStarted: 0, CreatedAt: 100, UpdatedAt: 100}
			if _, err := s.Append("goal/change", 1, 1, created); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(trial.eventType, 2, 0, trial.data); err != nil {
				t.Fatal(err)
			}
			if err := s.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := s.ValidateGoal(); err == nil || !strings.Contains(err.Error(), trial.want) {
				t.Fatalf("invalid persisted goal accepted: %v", err)
			}
		})
	}
}
