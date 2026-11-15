package goal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"go-harness/harness/session"
)

type nativeJournal struct{ events []sessionformat.Event }

func (j *nativeJournal) Append(_ context.Context, events []sessionformat.Event) error {
	if len(j.events) != 0 && events[0].Seq != uint64(len(j.events)) {
		return errors.New("non-contiguous native journal append")
	}
	j.events = append(j.events, events...)
	return nil
}

func (*nativeJournal) Close() error { return nil }

func nativeGoalSession(t *testing.T) (*sessionformat.LiveSession, *nativeJournal) {
	t.Helper()
	j := &nativeJournal{}
	s, err := sessionformat.NewLiveSession(sessionformat.Header{
		Type: "session", Version: 4, ID: "native-goal-test", CreatedAt: 1,
	}, j)
	if err != nil {
		t.Fatal(err)
	}
	return s, j
}

func TestNativeGoalServicePersistsV4AndColdResumeStartsDisarmed(t *testing.T) {
	ctx := context.Background()
	live, journal := nativeGoalSession(t)
	service, err := NewNative(live)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(ctx, "Finish report", 3)
	if err != nil || created.Goal.Phase != "active" || created.Activation != "armed" {
		t.Fatalf("created = %+v, %v", created, err)
	}
	if len(journal.events) != 1 || journal.events[0].Type != "goal/change" {
		t.Fatalf("durable goal events = %+v", journal.events)
	}
	change, err := sessionformat.DecodeGoalChange(journal.events[0].Data)
	if err != nil || change.Operation != "create" || change.Goal.ID != created.Goal.ID {
		t.Fatalf("v4 create = %+v, %v", change, err)
	}
	service.Disarm()
	resumed, err := service.Resume(ctx, sessionformat.GoalRef{ID: created.Goal.ID, Revision: 1})
	if err != nil || resumed.Goal.Revision != 2 || resumed.Activation != "armed" {
		t.Fatalf("resumed = %+v, %v", resumed, err)
	}
	paused, err := service.Pause(ctx, sessionformat.GoalRef{ID: created.Goal.ID, Revision: 2})
	if err != nil || paused.Goal.Phase != "paused" || paused.Activation != "disarmed" {
		t.Fatalf("paused = %+v, %v", paused, err)
	}
	artifact := sessionformat.PhysicalArtifact{Header: live.Header(), Events: live.DurableEvents()}
	restored, err := sessionformat.RestoreLiveSession(artifact, &nativeJournal{events: append([]sessionformat.Event(nil), journal.events...)})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	coldService, err := NewNative(restored)
	if err != nil {
		t.Fatal(err)
	}
	cold, found, err := coldService.Get()
	if err != nil || !found || cold.Goal.Phase != "paused" || cold.Activation != "disarmed" {
		t.Fatalf("cold goal = %+v, %v, %v", cold, found, err)
	}
	active, err := coldService.Resume(ctx, sessionformat.GoalRef{ID: cold.Goal.ID, Revision: cold.Goal.Revision})
	if err != nil || active.Goal.Revision != 4 || active.Activation != "armed" {
		t.Fatalf("cold resume = %+v, %v", active, err)
	}
	blocked, err := coldService.Block(ctx, sessionformat.GoalRef{ID: active.Goal.ID, Revision: 4}, sessionformat.GoalBlockReason{Code: "needs-input", Message: "Missing source"})
	if err != nil || blocked.Goal.Phase != "blocked" || blocked.Goal.BlockedReason == nil {
		t.Fatalf("blocked = %+v, %v", blocked, err)
	}
	completed, err := coldService.Complete(ctx, sessionformat.GoalRef{ID: blocked.Goal.ID, Revision: 5})
	if err != nil || completed.Goal.Phase != "complete" || completed.Goal.BlockedReason != nil {
		t.Fatalf("completed = %+v, %v", completed, err)
	}
	cleared, err := coldService.Clear(ctx, sessionformat.GoalRef{ID: completed.Goal.ID, Revision: 6})
	if err != nil || cleared.Revision != 7 {
		t.Fatalf("clear = %+v, %v", cleared, err)
	}
	if _, found, err := coldService.Get(); err != nil || found {
		t.Fatalf("clear did not remove current goal: found=%v err=%v", found, err)
	}
	if _, err := sessionformat.FoldGoal(restored.DurableEvents()); err != nil {
		t.Fatalf("durable v4 goal fold: %v", err)
	}
	if exportPath := os.Getenv("SERVERAGENTS_V4_GOAL_EXPORT_PATH"); exportPath != "" {
		var output bytes.Buffer
		header, err := sessionformat.EncodeHeader(live.Header())
		if err != nil {
			t.Fatal(err)
		}
		output.Write(header)
		output.WriteByte('\n')
		for index, event := range restored.DurableEvents() {
			line, err := sessionformat.EncodeEvent(event, uint64(index))
			if err != nil {
				t.Fatal(err)
			}
			output.Write(line)
			output.WriteByte('\n')
		}
		if err := os.WriteFile(exportPath, output.Bytes(), 0o600); err != nil {
			t.Fatalf("export native v4 goal journal: %v", err)
		}
	}
}

func TestNativeGoalServiceUsesUpstreamDefaultRoundCap(t *testing.T) {
	live, _ := nativeGoalSession(t)
	service, err := NewNative(live)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), "Finish report", 0)
	if err != nil || created.Goal.MaxGoalRounds != 256 {
		t.Fatalf("default goal round cap = %d, error=%v", created.Goal.MaxGoalRounds, err)
	}
}

func TestNativeGoalServiceRejectsInvalidMutationBeforeAppend(t *testing.T) {
	live, journal := nativeGoalSession(t)
	service, err := NewNative(live)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(context.Background(), "Finish report", 2)
	if err != nil {
		t.Fatal(err)
	}
	ref := sessionformat.GoalRef{ID: created.Goal.ID, Revision: 1}
	if _, err := service.Complete(context.Background(), sessionformat.GoalRef{ID: ref.ID, Revision: 2}); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale revision = %v", err)
	}
	if _, err := live.Append(sessionformat.EventInput{Type: "goal/change", Data: json.RawMessage(`{"kind":"goal/change","version":1,"operation":"clear","cleared":{"id":"wrong","revision":2},"clearedAt":9999999999999}`)}); err == nil {
		t.Fatal("invalid goal transition entered live Session")
	}
	if len(live.Events()) != 1 || len(journal.events) != 1 {
		t.Fatalf("invalid append changed log: live=%d durable=%d", len(live.Events()), len(journal.events))
	}
}
