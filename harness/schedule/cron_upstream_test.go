package schedule

import (
	"testing"
	"time"
)

// Keep the canonical spelling and star flags in sync with the upstream
// cron.spec.ts table: they affect Vixie day-field semantics after a restart.
func TestCronOfficialCanonicalExpressions(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"0 0 * * *", "0 0 * * *"},
		{"*/15 9-17 * * 1-5", "*/15 9-17 * * 1-5"},
		{"30,10,20 * * * *", "10-30/10 * * * *"},
		{"20,30,10 * * * *", "10-30/10 * * * *"},
		{"0,0,30 * * * *", "0-30/30 * * * *"},
		{"*/1 * * * *", "* * * * *"},
		{"0 0 */2 * *", "0 0 */2 * *"},
		{"0-59/1 0 * * *", "0-59 0 * * *"},
		{"0-59 0-23 1-31 1-12 0-7", "0-59 0-23 1-31 1-12 0-6"},
		{"0 0 * * 7", "0 0 * * 0"},
		{"0 0 * * 1-2,3-4", "0 0 * * 1-4"},
		{"0 0 * * 5-7", "0 0 * * 0,5-6"},
		{"1,3,5 * * * *", "1-5/2 * * * *"},
		{"00 09 * * 5,4,3,2,1", "0 9 * * 1-5"},
		{"0 0 1-15/2 * *", "0 0 1-15/2 * *"},
		{"0 0 * * */7", "0 0 * * */7"},
		{"0 9 1-31 * */7", "0 9 1-31 * */7"},
		{"0 9 */2,4 * *", "0 9 */2,4 * *"},
		{"0 9 1-31/2 * 1", "0 9 1-31/2 * 1"},
		{"*,5 0 * * *", "* 0 * * *"},
		{"5,* 0 * * *", "0-59 0 * * *"},
	} {
		t.Run(test.input, func(t *testing.T) {
			_, got, err := parseCron(test.input)
			if err != nil || got != test.want {
				t.Fatalf("canonical = %q, %v; want %q", got, err, test.want)
			}
			_, again, err := parseCron(got)
			if err != nil || again != got {
				t.Fatalf("canonical is not stable: %q, %v", again, err)
			}
		})
	}
}

func TestCronOfficialCanonicalRuleKeepsOccurrence(t *testing.T) {
	for _, expression := range []string{
		"0 9 1-31 * */7", "0 9 */31 * 1", "0 9 */2 * 1", "0 9 * * */7",
		"0 9 * * 1", "0 9 */2,4 * *", "0 9 1-31/2 * 1", "0 0 1-31 * 5",
	} {
		_, canonical, err := parseCron(expression)
		if err != nil {
			t.Fatal(err)
		}
		for _, floor := range []string{"2026-09-02T00:00:00Z", "2026-09-13T00:00:00Z", "2026-09-20T00:00:00Z"} {
			now, err := time.Parse(time.RFC3339, floor)
			if err != nil {
				t.Fatal(err)
			}
			record, err := NewCron("canonical", "Canonical", "Check", CronInput{Expression: expression, TimeZone: "UTC"}, now)
			if err != nil {
				t.Fatal(err)
			}
			decision := now.Add(24 * time.Hour)
			if scheduled, parseErr := time.Parse("2006-01-02T15:04:05.000Z", record.ScheduledAt); parseErr == nil && !scheduled.Before(decision) {
				decision = scheduled.Add(time.Millisecond)
			}
			raw := record
			raw.Expression = expression
			stored := record
			stored.Expression = canonical
			before, beforeErr := ResolveRecurringOccurrence(raw, decision)
			after, afterErr := ResolveRecurringOccurrence(stored, decision)
			if beforeErr != nil || afterErr != nil || before != after {
				t.Fatalf("%q at %s: raw %+v/%v, stored %+v/%v", expression, floor, before, beforeErr, after, afterErr)
			}
		}
	}
}

// Cases are taken from deepseek-harness packages/schedule/schedule/tests/cron.spec.ts
// at d743267388641bc76f17c45ce8b4c231aed1d32c.
func TestCronOfficialBoundaryCases(t *testing.T) {
	parse := func(value string) time.Time {
		t.Helper()
		instant, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			t.Fatal(err)
		}
		return instant
	}
	for _, test := range []struct {
		name, createdAt, expression, zone, decisionAt, wantTarget, wantOccurrence, wantNext string
	}{
		{"DST overlap before rollback", "2026-10-25T00:00:00Z", "30 1 * 11 0", "America/New_York", "2026-11-01T05:30:00Z", "2026-11-01T05:30:00.000Z", "2026-11-01T05:30:00.000Z", "2026-11-08T06:30:00.000Z"},
		{"DST overlap after rollback", "2026-10-25T00:00:00Z", "30 1 * 11 0", "America/New_York", "2026-11-01T06:30:00Z", "2026-11-01T05:30:00.000Z", "2026-11-01T05:30:00.000Z", "2026-11-08T06:30:00.000Z"},
		{"local date after UTC date", "2026-09-15T00:00:00Z", "0 9 * * *", "Pacific/Auckland", "2026-09-15T22:00:00Z", "2026-09-15T21:00:00.000Z", "2026-09-15T21:00:00.000Z", "2026-09-16T21:00:00.000Z"},
		{"date-line local day", "2025-12-31T00:00:00Z", "30 0 * * *", "Pacific/Kiritimati", "2026-01-01T11:00:00Z", "2025-12-31T10:30:00.000Z", "2026-01-01T10:30:00.000Z", "2026-01-02T10:30:00.000Z"},
		{"four-digit floor", "0001-01-01T00:00:00Z", "0 0 * * *", "UTC", "0001-01-02T00:00:00Z", "0001-01-02T00:00:00.000Z", "0001-01-02T00:00:00.000Z", "0001-01-03T00:00:00.000Z"},
		{"negative offset on floor date", "0001-01-01T00:01:00Z", "* * * * *", "Etc/GMT+1", "0001-01-01T00:30:00Z", "0001-01-01T00:02:00.000Z", "0001-01-01T00:30:00.000Z", "0001-01-01T00:31:00.000Z"},
		{"four-digit ceiling", "9999-12-31T23:00:00Z", "* * * * *", "UTC", "9999-12-31T23:59:59.999Z", "9999-12-31T23:01:00.000Z", "9999-12-31T23:59:00.000Z", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := NewCron("official", "Official", "Check", CronInput{Expression: test.expression, TimeZone: test.zone}, parse(test.createdAt))
			if err != nil {
				t.Fatal(err)
			}
			if record.ScheduledAt != test.wantTarget {
				t.Fatalf("target = %s, want %s", record.ScheduledAt, test.wantTarget)
			}
			occurrence, err := ResolveRecurringOccurrence(record, parse(test.decisionAt))
			if err != nil {
				t.Fatal(err)
			}
			if occurrence.OccurrenceAt != test.wantOccurrence || occurrence.NextScheduledAt != test.wantNext {
				t.Fatalf("occurrence = %+v, want %s / %s", occurrence, test.wantOccurrence, test.wantNext)
			}
		})
	}
}

func TestCronOfficialPinnedAndImpossibleCases(t *testing.T) {
	created := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	record, err := NewCron("pinned", "Pinned", "Check", CronInput{Expression: "0 9,17 * * *", TimeZone: "UTC"}, created)
	if err != nil {
		t.Fatal(err)
	}
	record.ScheduledAt = "2026-09-15T12:00:00.000Z"
	for _, test := range []struct {
		at, occurrence, next string
	}{
		{"2026-09-15T12:30:00Z", "2026-09-15T12:00:00.000Z", "2026-09-15T17:00:00.000Z"},
		{"2026-09-15T17:30:00Z", "2026-09-15T17:00:00.000Z", "2026-09-16T09:00:00.000Z"},
	} {
		at, err := time.Parse(time.RFC3339, test.at)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ResolveRecurringOccurrence(record, at)
		if err != nil || got.OccurrenceAt != test.occurrence || got.NextScheduledAt != test.next {
			t.Errorf("pinned at %s = %+v, %v", test.at, got, err)
		}
	}
	impossible := Record{ID: "impossible", Kind: "cron", Title: "Impossible", Prompt: "Check", Expression: "0 0 30 2 *", TimeZone: "UTC", ScheduledAt: "2026-09-15T00:00:00.000Z"}
	got, err := ResolveRecurringOccurrence(impossible, time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC))
	if err != nil || got.OccurrenceAt != impossible.ScheduledAt || got.NextScheduledAt != "" {
		t.Fatalf("impossible recurrence = %+v, %v", got, err)
	}
}

func TestCronOfficialVixieDayRules(t *testing.T) {
	for _, test := range []struct {
		name, now, expression, want string
	}{
		{"Friday match", "2026-09-01T00:00:00Z", "0 0 13 * 5", "2026-09-04T00:00:00.000Z"},
		{"month day match", "2026-09-12T00:00:00Z", "0 0 13 * 5", "2026-09-13T00:00:00.000Z"},
		{"month day alone", "2026-09-01T00:00:00Z", "0 0 13 * *", "2026-09-13T00:00:00.000Z"},
		{"weekday alone", "2026-09-01T00:00:00Z", "0 0 * * 5", "2026-09-04T00:00:00.000Z"},
		{"both stars", "2026-09-01T00:00:00Z", "0 0 * * *", "2026-09-02T00:00:00.000Z"},
		{"explicit full month days", "2026-09-01T00:00:00Z", "0 0 1-31 * 5", "2026-09-02T00:00:00.000Z"},
		{"explicit full weekdays", "2026-09-01T00:00:00Z", "0 0 13 * 0-7", "2026-09-02T00:00:00.000Z"},
		{"stepped month days odd Monday", "2026-09-01T00:00:00Z", "0 9 */2 * 1", "2026-09-07T09:00:00.000Z"},
		{"stepped month days even Monday", "2026-09-13T00:00:00Z", "0 9 */2 * 1", "2026-09-21T09:00:00.000Z"},
		{"stepped weekday star", "2026-09-02T00:00:00Z", "0 9 1-31 * */2", "2026-09-03T09:00:00.000Z"},
		{"two stepped stars", "2026-09-02T00:00:00Z", "0 9 */2 * */2", "2026-09-03T09:00:00.000Z"},
		{"stepped month and bare weekday", "2026-09-02T00:00:00Z", "0 9 */2 * *", "2026-09-03T09:00:00.000Z"},
		{"bare month and stepped weekday", "2026-09-02T00:00:00Z", "0 9 * * */2", "2026-09-03T09:00:00.000Z"},
		{"bare month and restricted weekday", "2026-09-13T00:00:00Z", "0 9 * * 1", "2026-09-14T09:00:00.000Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, test.now)
			if err != nil {
				t.Fatal(err)
			}
			record, err := NewCron("vixie", "Vixie", "Check", CronInput{Expression: test.expression, TimeZone: "UTC"}, now)
			if err != nil || record.ScheduledAt != test.want {
				t.Fatalf("%s = %+v, %v; want %s", test.expression, record, err, test.want)
			}
		})
	}
}
