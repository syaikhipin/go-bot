package schedule

import (
	"testing"
	"time"
)

func TestCronVixieDayCombination(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC) // Saturday
	// Both restricted day fields use Vixie's OR rule: this Monday qualifies
	// through day-of-week even though it is not the first day of the month.
	record, err := NewCron("c", "Weekly", "Check", CronInput{
		Expression: "0 9 1 * 1", TimeZone: "UTC",
	}, now)
	if err != nil || record.ScheduledAt != "2026-10-12T09:00:00.000Z" {
		t.Fatalf("restricted day rule = %+v, %v", record, err)
	}
	// A star in the weekday field switches to AND, leaving the month-day
	// restriction effective.
	record, err = NewCron("c", "Monthly", "Check", CronInput{
		Expression: "0 9 1 * *", TimeZone: "UTC",
	}, now)
	if err != nil || record.ScheduledAt != "2026-11-01T09:00:00.000Z" {
		t.Fatalf("star day rule = %+v, %v", record, err)
	}
}

func TestCronDialectRejectsExtensionsAndUsesEarlierOverlap(t *testing.T) {
	now := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)
	record, err := NewCron("c", "Clock", "Check", CronInput{
		Expression: "30 1 * * *", TimeZone: "America/New_York",
	}, now)
	if err != nil || record.ScheduledAt != "2026-11-01T05:30:00.000Z" {
		t.Fatalf("overlap = %+v, %v", record, err)
	}
	for _, expression := range []string{
		"@daily", "0 9 * * MON", "0 9 * * * *", "0 9 L * *", "*/0 9 * * *", "0 9 31-1 * *",
		"999999999999999999999-59 9 * * *", "0-999999999999999999999 9 * * *",
		"1-2/9223372036854775808 9 * * *",
		"*/9007199254740992 9 * * *", "1-2/9007199254740992 9 * * *",
	} {
		if _, err := NewCron("c", "Clock", "Check", CronInput{Expression: expression, TimeZone: "UTC"}, now); err == nil {
			t.Errorf("unsupported cron expression accepted: %q", expression)
		}
	}
}

func TestCronCanonicalizationPreservesVixieStarFlag(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"0,0,15 9 * * 7", "0-15/15 9 * * 0"},
		{"0 9 1-31 * 1", "0 9 1-31 * 1"},
		{"*/2 9 * * 1", "*/2 9 * * 1"},
		{"0 9 */2,3 * 1", "0 9 */2 * 1"},
	} {
		_, got, err := parseCron(test.input)
		if err != nil || got != test.want {
			t.Errorf("canonicalize %q = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
}
