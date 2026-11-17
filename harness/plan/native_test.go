package plan

import (
	"context"
	"testing"

	"go-bot/harness/session"
)

type planJournal struct{ events []sessionformat.Event }

func (j *planJournal) Append(_ context.Context, events []sessionformat.Event) error {
	j.events = append(j.events, events...)
	return nil
}

func (*planJournal) Close() error { return nil }

func TestNativePlanSelectionCommitsAtStepAndRestoresFromV4(t *testing.T) {
	ctx := context.Background()
	journal := &planJournal{}
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "plan-v4", CreatedAt: 1}, journal)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewNative(live)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetNow(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := service.Queue(false); err != nil {
		t.Fatal(err)
	}
	if view, err := service.View(); err != nil || !view.Active || view.Pending == nil || *view.Pending {
		t.Fatalf("pending plan selection = %+v, %v", view, err)
	}
	if len(journal.events) != 1 || journal.events[0].Type != "plan/mode" {
		t.Fatalf("pending plan selection committed early: %+v", journal.events)
	}
	if err := service.ApplyPending(ctx); err != nil {
		t.Fatal(err)
	}
	if len(journal.events) != 2 || journal.events[1].Type != "plan/mode" {
		t.Fatalf("applied plan selection not durable: %+v", journal.events)
	}
	artifact := sessionformat.PhysicalArtifact{Header: live.Header(), Events: live.DurableEvents()}
	restored, err := sessionformat.RestoreLiveSession(artifact, &planJournal{events: append([]sessionformat.Event(nil), journal.events...)})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	cold, err := NewNative(restored)
	if err != nil {
		t.Fatal(err)
	}
	if view, err := cold.View(); err != nil || view.Active || view.Pending != nil {
		t.Fatalf("restored plan mode = %+v, %v", view, err)
	}
}
