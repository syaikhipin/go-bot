package proactive

import (
	"testing"
	"time"
)

func TestTimedWakeDoesNotWaitForChangeWindow(t *testing.T) {
	start := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	s := State{LastRunStartedAt: start.UnixMilli(), NextCheck: &Check{At: start.Add(15 * time.Minute).Format(time.RFC3339), Reason: "AI decision"}}
	s.Changed(start.Add(8 * time.Minute))
	if got := s.ChangeEligibleAt(); !got.Equal(start.Add(30 * time.Minute)) {
		t.Fatalf("change eligible at %s", got)
	}
	if at, reason := s.NextWake(); !at.Equal(start.Add(15*time.Minute)) || reason != TimedWake {
		t.Fatalf("timed decision was blocked by the change gate: %s %s", at, reason)
	}
	// The independent timed run consumes old pending information after its
	// successful collection. It does not create a new quiet-window timestamp.
	s.LastRunStartedAt = start.Add(15 * time.Minute).UnixMilli()
	s.PendingChange = false
	s.NextCheck = &Check{At: start.Add(4 * time.Hour).Format(time.RFC3339), Reason: "Next publication"}
	if at, _ := s.NextWake(); !at.Equal(start.Add(4 * time.Hour)) {
		t.Fatalf("old marker produced an extra wake at %s", at)
	}
	s.Changed(start.Add(20 * time.Minute))
	if at, reason := s.NextWake(); !at.Equal(start.Add(45*time.Minute)) || reason != ChangeWake {
		t.Fatalf("new information should use the latest actual run: %s %s", at, reason)
	}
	if s.LastChangeAt != start.Add(20*time.Minute).UnixMilli() {
		t.Fatal("running the Agent changed the information timestamp")
	}
}

func TestStabilityResetsOnInformationNotOnPolling(t *testing.T) {
	start := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	s := State{}
	if at, _ := s.NextWake(); !at.IsZero() {
		t.Fatal("no information and no AI decision should invent a periodic timer")
	}
	s.Changed(start)
	if got := s.ChangeEligibleAt(); !got.Equal(start.Add(10 * time.Minute)) {
		t.Fatalf("first change should wait only for stability: %s", got)
	}
	s.Changed(start.Add(9 * time.Minute))
	for range 3 {
		if got := s.ChangeEligibleAt(); !got.Equal(start.Add(19 * time.Minute)) {
			t.Fatalf("reading eligibility changed the quiet window: %s", got)
		}
	}
}

func TestFailedEarlierChangeCheckKeepsUnattemptedAITimer(t *testing.T) {
	start := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	s := State{LastRunStartedAt: start.UnixMilli(), LastError: "background_turn_failed", NextCheck: &Check{At: start.Add(time.Hour).Format(time.RFC3339), Reason: "AI chose the publication time"}}
	if at, reason := s.NextWake(); !at.Equal(start.Add(time.Hour)) || reason != TimedWake {
		t.Fatalf("failed earlier check cancelled the future AI timer: %s %s", at, reason)
	}
	s.LastRunStartedAt = start.Add(time.Hour).UnixMilli()
	if at, _ := s.NextWake(); !at.IsZero() {
		t.Fatalf("failed overdue timer would spin at %s", at)
	}
}
