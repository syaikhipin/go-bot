package schedule

import (
	"testing"
	"time"
)

func TestEveryMissedOccurrencesCollapseToLatest(t *testing.T) {
	start := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	record, err := NewEvery("e", "Queue", "Check", 60, start)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRecurringOccurrence(record, start.Add(5*time.Minute+15*time.Second))
	if err != nil || got.OccurrenceAt != "2026-10-10T00:05:00.000Z" || got.NextScheduledAt != "2026-10-10T00:06:00.000Z" {
		t.Fatalf("occurrence = %+v, %v", got, err)
	}
}

func TestDailyMissedDatesAndDSTGap(t *testing.T) {
	start := time.Date(2026, 3, 7, 5, 0, 0, 0, time.UTC)
	record, err := NewDaily("d", "Daily", "Check", LocalTimeInput{Time: "02:30:00", TimeZone: "America/New_York"}, start)
	if err != nil {
		t.Fatal(err)
	}
	// March 8's 02:30 never existed. March 9 is the latest valid target.
	got, err := ResolveRecurringOccurrence(record, time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC))
	if err != nil || got.OccurrenceAt != "2026-03-09T06:30:00.000Z" || got.NextScheduledAt != "2026-03-10T06:30:00.000Z" {
		t.Fatalf("daily occurrence = %+v, %v", got, err)
	}
}

func TestDailyUpstreamHistoricalCalendarTransitions(t *testing.T) {
	parse := func(value string) time.Time {
		t.Helper()
		instant, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatal(err)
		}
		return instant
	}
	for _, test := range []struct {
		name, start, clock, zone, target, decision, occurrence, next string
	}{
		{
			name: "Anchorage historical date-line rollback", start: "1867-10-17T00:00:00Z",
			clock: "12:00:00", zone: "America/Anchorage", target: "1867-10-17T21:59:36.000Z",
			decision: "1867-10-19T06:00:00Z", occurrence: "1867-10-18T21:59:36.000Z", next: "1867-10-20T21:59:36.000Z",
		},
		{
			name: "Apia skips December 30", start: "2011-12-29T21:59:59Z",
			clock: "12:00:00", zone: "Pacific/Apia", target: "2011-12-29T22:00:00.000Z",
			decision: "2011-12-29T22:00:00Z", occurrence: "2011-12-29T22:00:00.000Z", next: "2011-12-30T22:00:00.000Z",
		},
		{
			name: "Lord Howe half-hour overlap", start: "2026-04-04T13:00:00Z",
			clock: "01:45:00", zone: "Australia/Lord_Howe", target: "2026-04-04T14:45:00.000Z",
			decision: "2026-04-04T15:10:00Z", occurrence: "2026-04-04T14:45:00.000Z", next: "2026-04-05T15:15:00.000Z",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := NewDaily("d", "Daily", "Check", LocalTimeInput{Time: test.clock, TimeZone: test.zone}, parse(test.start))
			if err != nil || record.ScheduledAt != test.target {
				t.Fatalf("daily target = %q, %v; want %q", record.ScheduledAt, err, test.target)
			}
			got, err := ResolveRecurringOccurrence(record, parse(test.decision))
			if err != nil || got.OccurrenceAt != test.occurrence || got.NextScheduledAt != test.next {
				t.Fatalf("daily decision = %+v, %v; want occurrence %q next %q", got, err, test.occurrence, test.next)
			}
		})
	}
	if _, err := NewLocalAt("a", "Gap", "Check", LocalAtInput{
		Date: "2011-12-30", Time: "12:00:00", TimeZone: "Pacific/Apia",
	}, parse("2011-12-29T00:00:00Z")); err == nil {
		t.Fatal("accepted a local time on Apia's skipped December 30")
	}
	// The supported range applies to UTC instants, not the local calendar
	// year. In UTC+1, 10000-01-01 00:30 is still 9999-12-31 UTC.
	localYearTenThousand, err := NewDaily("d", "Daily", "Check", LocalTimeInput{
		Time: "00:30:00", TimeZone: "Etc/GMT-1",
	}, parse("9999-12-31T23:00:00Z"))
	if err != nil || localYearTenThousand.ScheduledAt != "9999-12-31T23:30:00.000Z" {
		t.Fatalf("local year 10000 target = %+v, %v", localYearTenThousand, err)
	}
	beforeFinalLocalDate, err := NewDaily("d", "Daily", "Check", LocalTimeInput{
		Time: "00:30:00", TimeZone: "Etc/GMT-1",
	}, parse("9999-12-30T23:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRecurringOccurrence(beforeFinalLocalDate, parse(beforeFinalLocalDate.ScheduledAt))
	if err != nil || got.NextScheduledAt != "9999-12-31T23:30:00.000Z" {
		t.Fatalf("next target in local year 10000 = %+v, %v", got, err)
	}
	if _, err := NewDaily("d", "Daily", "Check", LocalTimeInput{
		Time: "01:30:00", TimeZone: "Etc/GMT-1",
	}, parse("9999-12-31T23:00:00Z")); err == nil {
		t.Fatal("accepted a local year 10000 target outside UTC year 9999")
	}
}

func TestCronMissedOccurrencesCollapseToLatest(t *testing.T) {
	start := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	record, err := NewCron("c", "Check", "Queue", CronInput{Expression: "*/15 * * * *", TimeZone: "UTC"}, start)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRecurringOccurrence(record, start.Add(2*time.Hour+22*time.Minute))
	if err != nil || got.OccurrenceAt != "2026-10-10T02:15:00.000Z" || got.NextScheduledAt != "2026-10-10T02:30:00.000Z" {
		t.Fatalf("cron occurrence = %+v, %v", got, err)
	}
}

func TestCronLatestDueAcrossDSTOverlap(t *testing.T) {
	// Upstream cron.spec.ts: 01:45 EDT is due even when the decision is
	// 01:30 EST, which is earlier by wall clock but later by instant.
	start := time.Date(2026, 10, 25, 5, 45, 0, 0, time.UTC)
	record, err := NewCron("c", "Clock", "Check", CronInput{
		Expression: "45 1 * * 0", TimeZone: "America/New_York",
	}, start)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRecurringOccurrence(record, time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC))
	if err != nil || got.OccurrenceAt != "2026-11-01T05:45:00.000Z" || got.NextScheduledAt != "2026-11-08T06:45:00.000Z" {
		t.Fatalf("DST overlap occurrence = %+v, %v", got, err)
	}
}

func TestCronLatestDueInLocalDateAfterUTCDate(t *testing.T) {
	// In UTC+14 the due 00:30 minute falls on the local day after the
	// decision's UTC date. The backward search must include that date.
	start := time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)
	record, err := NewCron("c", "Clock", "Check", CronInput{
		Expression: "30 0 * * *", TimeZone: "Pacific/Kiritimati",
	}, start)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRecurringOccurrence(record, time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC))
	if err != nil || got.OccurrenceAt != "2026-01-01T10:30:00.000Z" || got.NextScheduledAt != "2026-01-02T10:30:00.000Z" {
		t.Fatalf("UTC+14 occurrence = %+v, %v", got, err)
	}
}

func TestCalendarRuleRejectsCreationTimeOutsideFourDigitUTCYears(t *testing.T) {
	// A next target in year 0001 must not make an acceptance instant in
	// year 0000 valid. The upstream domain checks now before target search.
	beforeFloor := time.Date(0, 12, 31, 23, 59, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		create func() (Record, error)
	}{
		{"daily", func() (Record, error) {
			return NewDaily("d", "Daily", "Check", LocalTimeInput{Time: "00:00:00", TimeZone: "UTC"}, beforeFloor)
		}},
		{"weekly", func() (Record, error) {
			return NewWeekly("w", "Weekly", "Check", LocalTimeInput{Time: "00:00:00", TimeZone: "UTC", Weekdays: []int{1}}, beforeFloor)
		}},
		{"cron", func() (Record, error) {
			return NewCron("c", "Cron", "Check", CronInput{Expression: "0 0 * * *", TimeZone: "UTC"}, beforeFloor)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if record, err := test.create(); err == nil {
				t.Fatalf("accepted year 0000 creation instant: %+v", record)
			}
		})
	}
}

func TestCronCanScheduleLocalYearTenThousandInsideUTCYear9999(t *testing.T) {
	zone := CronInput{Expression: "30 0 * * *", TimeZone: "Etc/GMT-1"}
	final, err := NewCron("c", "Cron", "Check", zone, time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC))
	if err != nil || final.ScheduledAt != "9999-12-31T23:30:00.000Z" {
		t.Fatalf("cron local year 10000 target = %+v, %v", final, err)
	}
	prior, err := NewCron("c", "Cron", "Check", zone, time.Date(9999, 12, 30, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRecurringOccurrence(prior, time.Date(9999, 12, 30, 23, 30, 0, 0, time.UTC))
	if err != nil || got.NextScheduledAt != final.ScheduledAt {
		t.Fatalf("cron next target in local year 10000 = %+v, %v", got, err)
	}
	if _, err := NewCron("c", "Cron", "Check", CronInput{
		Expression: "30 1 * * *", TimeZone: "Etc/GMT-1",
	}, time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("accepted a cron target outside UTC year 9999")
	}
}

func TestWeeklyCanScheduleLocalYearTenThousandInsideUTCYear9999(t *testing.T) {
	weekday := int(time.Date(10000, 1, 1, 12, 0, 0, 0, time.UTC).Weekday())
	if weekday == 0 {
		weekday = 7
	}
	record, err := NewWeekly("w", "Weekly", "Check", LocalTimeInput{
		Time: "00:30:00", TimeZone: "Etc/GMT-1", Weekdays: []int{weekday},
	}, time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC))
	if err != nil || record.ScheduledAt != "9999-12-31T23:30:00.000Z" {
		t.Fatalf("weekly local year 10000 target = %+v, %v", record, err)
	}
}
