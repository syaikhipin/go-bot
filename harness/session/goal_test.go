package sessionformat

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func nativeGoalChangeEvent(t *testing.T, seq uint64, change GoalChange) Event {
	t.Helper()
	data, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	return Event{Type: "goal/change", Seq: seq, Time: int64(seq + 1), Data: data}
}

func nativeGoalSnapshot(id string, revision uint64, phase string) GoalSnapshot {
	return GoalSnapshot{ID: id, Revision: revision, Objective: "Finish the report", Phase: phase, MaxGoalRounds: 3}
}

func TestNativeGoalFoldMatchesDeepSeekRevisionAndRoundRules(t *testing.T) {
	makeChange := func(operation string, goal GoalSnapshot, rounds uint64, created, updated int64) GoalChange {
		return GoalChange{Kind: "goal/change", Version: 1, Operation: operation, Goal: &goal, RoundsStarted: rounds, CreatedAt: created, UpdatedAt: updated}
	}
	create := makeChange("create", nativeGoalSnapshot("g", 1, "active"), 0, 10, 10)
	events := []Event{nativeGoalChangeEvent(t, 0, create)}
	events = append(events, Event{Type: "user/message", Seq: 1, Time: 11,
		Data: json.RawMessage(`{"id":"round-1","role":"user","source":{"kind":"goal","goalId":"g","revision":1,"round":1},"content":[{"type":"text","text":"continue"}]}`), SurfaceOp: json.RawMessage(`"append"`)})
	paused := nativeGoalSnapshot("g", 2, "paused")
	events = append(events, nativeGoalChangeEvent(t, 2, makeChange("pause", paused, 1, 10, 12)))
	resumed := nativeGoalSnapshot("g", 3, "active")
	events = append(events, nativeGoalChangeEvent(t, 3, makeChange("resume", resumed, 1, 10, 13)))
	blocked := nativeGoalSnapshot("g", 4, "blocked")
	blocked.BlockedReason = &GoalBlockReason{Code: "needs-input", Message: "Need the source file"}
	events = append(events, nativeGoalChangeEvent(t, 4, makeChange("block", blocked, 1, 10, 14)))
	edited := blocked
	edited.Revision = 5
	edited.Objective = "Finish the revised report"
	events = append(events, nativeGoalChangeEvent(t, 5, makeChange("edit", edited, 1, 10, 15)))
	active := edited
	active.Revision, active.Phase, active.BlockedReason = 6, "active", nil
	events = append(events, nativeGoalChangeEvent(t, 6, makeChange("resume", active, 1, 10, 16)))
	completed := active
	completed.Revision, completed.Phase = 7, "complete"
	events = append(events, nativeGoalChangeEvent(t, 7, makeChange("complete", completed, 1, 10, 17)))
	state, err := FoldGoal(events)
	if err != nil || state.Goal == nil || state.Goal.Revision != 7 || state.Goal.Phase != "complete" || state.RoundsStarted != 1 || state.LastRef.Revision != 7 {
		t.Fatalf("native goal projection = %+v, %v", state, err)
	}
	second := nativeGoalSnapshot("g2", 1, "active")
	events = append(events, nativeGoalChangeEvent(t, 8, makeChange("create", second, 0, 18, 18)))
	clear := GoalChange{Kind: "goal/change", Version: 1, Operation: "clear", Cleared: &GoalRef{ID: "g2", Revision: 2}, ClearedAt: 19}
	events = append(events, nativeGoalChangeEvent(t, 9, clear))
	state, err = FoldGoal(events)
	if err != nil || state.Goal != nil || state.RoundsStarted != 0 || state.LastRef == nil || state.LastRef.ID != "g2" || state.LastRef.Revision != 2 {
		t.Fatalf("native clear tombstone = %+v, %v", state, err)
	}
	reuse := append(append([]Event(nil), events...), nativeGoalChangeEvent(t, 10, makeChange("create", second, 0, 20, 20)))
	if _, err := FoldGoal(reuse); err == nil {
		t.Fatal("cleared goal ID was reused")
	}
}

func TestNativeGoalFoldRejectsMalformedAndOutOfOrderFacts(t *testing.T) {
	for _, raw := range []string{
		`{"kind":"goal/change","version":1,"operation":"create","goal":{"id":"g","revision":1,"objective":"x","phase":"active","maxGoalRounds":2},"createdAt":1,"updatedAt":1}`,
		`{"kind":"goal/change","version":1,"operation":"create","goal":{"id":"g","revision":1,"objective":" x ","phase":"active","maxGoalRounds":2},"roundsStarted":0,"createdAt":1,"updatedAt":1}`,
		`{"kind":"goal/change","version":1,"operation":"create","goal":{"id":"g","revision":1,"objective":"x","phase":"active","maxGoalRounds":2,"extra":true},"roundsStarted":0,"createdAt":1,"updatedAt":1}`,
		`{"kind":"goal/change","version":2,"operation":"clear","cleared":{"id":"g","revision":2},"clearedAt":2}`,
		`{"kind":"goal/change","version":1,"operation":"clear","cleared":{"id":"g","revision":2},"clearedAt":2,"goal":null}`,
	} {
		if _, err := DecodeGoalChange(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid native goal payload %s", raw)
		}
	}
	create := nativeGoalChangeEvent(t, 0, GoalChange{Kind: "goal/change", Version: 1, Operation: "create", Goal: ptrNativeGoal(nativeGoalSnapshot("g", 1, "active")), CreatedAt: 1, UpdatedAt: 1})
	wrongRound := Event{Type: "user/message", Seq: 1, Time: 2, Data: json.RawMessage(`{"source":{"kind":"goal","goalId":"g","revision":1,"round":2}}`)}
	if _, err := FoldGoal([]Event{create, wrongRound}); err == nil || !strings.Contains(err.Error(), "next admitted round") {
		t.Fatalf("out-of-order native goal round accepted: %v", err)
	}
	wrongPause := nativeGoalSnapshot("g", 3, "paused")
	if _, err := FoldGoal([]Event{create, nativeGoalChangeEvent(t, 1, GoalChange{Kind: "goal/change", Version: 1, Operation: "pause", Goal: &wrongPause, CreatedAt: 1, UpdatedAt: 2})}); err == nil {
		t.Fatal("skipped goal revision was accepted")
	}
}

func ptrNativeGoal(goal GoalSnapshot) *GoalSnapshot { return &goal }

func TestNativeGoalColdRestoreValidatesDurableGoalChange(t *testing.T) {
	ctx := context.Background()
	store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: "/root/harness/workspaces/goal-session", Claim: v4Claim}
	header := v4TestHeader("goal-session")
	header.CWD = store.ProjectCWD
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	change := GoalChange{Kind: "goal/change", Version: 1, Operation: "create", Goal: ptrNativeGoal(nativeGoalSnapshot("g", 1, "active")), CreatedAt: 1, UpdatedAt: 1}
	if err := journal.Append(ctx, []Event{nativeGoalChangeEvent(t, 0, change)}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, artifact, err := store.Open(ctx, "goal-session")
	if err != nil {
		t.Fatal(err)
	}
	live, err := RestoreLiveSession(artifact, reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	state, err := FoldGoal(live.DurableEvents())
	if err != nil || state.Goal == nil || state.Goal.ID != "g" || state.RoundsStarted != 0 {
		t.Fatalf("restored native goal = %+v, %v", state, err)
	}
}
