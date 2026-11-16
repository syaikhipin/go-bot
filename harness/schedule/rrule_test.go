package schedule

import (
	"testing"
	"time"
)

// Fixtures follow the pinned upstream Schedule and time RRULE tests.
func TestRRuleMatchesUpstreamOccurrences(t *testing.T) {
	for _, tc := range []struct {
		name, rule, now, first, decision, latest, next string
	}{
		{"daily", "DTSTART:20260916T010000Z\nRRULE:FREQ=DAILY", "2026-09-16T00:00:00Z", "2026-09-16T01:00:00.000Z", "2026-09-18T12:00:00Z", "2026-09-18T01:00:00.000Z", "2026-09-19T01:00:00.000Z"},
		{"last weekday", "DTSTART:20260916T010000Z\nRRULE:FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1", "2026-09-16T00:00:00Z", "2026-09-30T01:00:00.000Z", "2026-10-31T12:00:00Z", "2026-10-30T01:00:00.000Z", "2026-11-30T01:00:00.000Z"},
		{"DST gap preserves count", "DTSTART;TZID=America/New_York:20260307T023000\nRRULE:FREQ=DAILY;COUNT=2", "2026-03-07T00:00:00Z", "2026-03-07T07:30:00.000Z", "2026-03-10T12:00:00Z", "2026-03-09T06:30:00.000Z", ""},
		{"DST overlap chooses earlier", "DTSTART;TZID=America/New_York:20261101T013000\nRRULE:FREQ=DAILY;COUNT=1", "2026-11-01T00:00:00Z", "2026-11-01T05:30:00.000Z", "2026-11-02T12:00:00Z", "2026-11-01T05:30:00.000Z", ""},
		{"rebases after years", "DTSTART:20000101T000000Z\nRRULE:FREQ=MINUTELY;INTERVAL=3", "2026-09-16T01:00:00Z", "2026-09-16T01:03:00.000Z", "2099-09-16T12:01:00Z", "2099-09-16T12:00:00.000Z", "2099-09-16T12:03:00.000Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, _ := time.Parse(time.RFC3339, tc.now)
			r, err := NewRRule("id", "Title", "Prompt", tc.rule, now)
			if err != nil || r.ScheduledAt != tc.first {
				t.Fatalf("first = %q, %v", r.ScheduledAt, err)
			}
			when, _ := time.Parse(time.RFC3339, tc.decision)
			got, err := ResolveRecurringOccurrence(r, when)
			if err != nil || got.OccurrenceAt != tc.latest || got.NextScheduledAt != tc.next {
				t.Fatalf("resolved = %+v, %v", got, err)
			}
		})
	}
}

func TestRRuleRejectsUnsafeOrUnsupportedRules(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, line := range []string{
		"FREQ=SECONDLY", "FREQ=MINUTELY;BYDAY=MO", "FREQ=DAILY;COUNT=0", "FREQ=DAILY;COUNT=1001",
		"FREQ=DAILY;COUNT=1;UNTIL=20260930T010000Z", "FREQ=DAILY;BYSECOND=1,2",
		"FREQ=DAILY;BYDAY=MO,TU;BYSETPOS=-1", "FREQ=MONTHLY;BYDAY=MO,MO", "FREQ=DAILY;BYEASTER=1",
	} {
		if _, err := NewRRule("id", "Title", "Prompt", "DTSTART:20260916T010000Z\nRRULE:"+line, now); err == nil {
			t.Errorf("accepted %s", line)
		}
	}
}

func TestRRuleTimeZoneAliasKeepsRuleEquivalent(t *testing.T) {
	now := time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC)
	alias := "DTSTART;TZID=US/Eastern:20260307T023000\nRRULE:FREQ=DAILY;COUNT=2"
	canonical := "DTSTART;TZID=America/New_York:20260307T023000\nRRULE:FREQ=DAILY;COUNT=2"
	record, err := NewRRule("id", "Title", "Prompt", alias, now)
	if err != nil || record.RRule != canonical || record.ScheduledAt != "2026-03-07T07:30:00.000Z" {
		t.Fatalf("created rule = %+v, %v", record, err)
	}
	equivalent, err := EquivalentRRule(record, alias)
	if err != nil || !equivalent {
		t.Fatalf("alias equivalent = %t, %v", equivalent, err)
	}
}
