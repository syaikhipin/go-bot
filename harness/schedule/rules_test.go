package schedule

import (
	"strings"
	"testing"
	"time"
)

func TestOneShotAndFixedIntervalRules(t *testing.T) {
	now := time.Date(2026, 10, 10, 9, 30, 0, 0, time.UTC)
	after, err := NewAfter("a", " Build ", " Check it ", 90, now)
	if err != nil || after.Title != "Build" || after.Prompt != "Check it" || after.ScheduledAt != "2026-10-10T09:31:30.000Z" {
		t.Fatalf("after = %+v, %v", after, err)
	}
	if _, err := NewAfter("a", "Build", "Check it", 0, now); err == nil {
		t.Fatal("zero-delay reminder accepted")
	}
	if _, err := NewEvery("e", "Build", "Check it", 59, now); err == nil {
		t.Fatal("sub-minute fixed interval accepted")
	}
	every, err := NewEvery("e", "Build", "Check it", 60, now)
	if err != nil || every.ScheduledAt != "2026-10-10T09:31:00.000Z" {
		t.Fatalf("every = %+v, %v", every, err)
	}
	at, err := NewAt("t", "Build", "Check it", "2026-10-10T18:00:00+08:00", now)
	if err != nil || at.ScheduledAt != "2026-10-10T10:00:00.000Z" {
		t.Fatalf("at = %+v, %v", at, err)
	}
	if _, err := NewAt("t", "Build", "Check it", "2026-10-10T11:00:00", now); err == nil {
		t.Fatal("zone-less at accepted")
	}
	for _, invalid := range []string{
		"2026-10-10T11:00:00.1234Z",
		"2026-10-10T11:00:00-00:00",
		"2026-10-10T11:00:00+24:00",
	} {
		if _, err := NormalizeAt(invalid); err == nil {
			t.Errorf("invalid at instant accepted: %s", invalid)
		}
	}
}

func TestDailyDSTGapAndEarlierOverlap(t *testing.T) {
	input := LocalTimeInput{Time: "02:30:00", TimeZone: "America/New_York"}
	beforeGap := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)
	record, err := NewDaily("d", "Daily", "Check", input, beforeGap)
	if err != nil || record.ScheduledAt != "2026-03-09T06:30:00.000Z" {
		t.Fatalf("gap = %+v, %v", record, err)
	}
	input.Time = "01:30:00"
	beforeOverlap := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)
	record, err = NewDaily("d", "Daily", "Check", input, beforeOverlap)
	if err != nil || record.ScheduledAt != "2026-11-01T05:30:00.000Z" {
		t.Fatalf("overlap = %+v, %v", record, err)
	}
	// At 05:45Z the earlier 01:30 occurrence has passed; the later
	// duplicate on the same local date must not count as a second day.
	record, err = NewDaily("d", "Daily", "Check", input, time.Date(2026, 11, 1, 5, 45, 0, 0, time.UTC))
	if err != nil || record.ScheduledAt != "2026-11-02T06:30:00.000Z" {
		t.Fatalf("second overlap = %+v, %v", record, err)
	}
}

func TestScheduleZoneAliasesUseIntlCanonicalName(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct{ input, canonical string }{
		{"US/Eastern", "America/New_York"},
		{"Asia/Kolkata", "Asia/Calcutta"},
		{"Europe/Kyiv", "Europe/Kiev"},
	} {
		record, err := NewDaily("d", "Daily", "Check", LocalTimeInput{Time: "09:00:00", TimeZone: test.input}, now)
		if err != nil || record.TimeZone != test.canonical {
			t.Errorf("daily zone %s = %q, %v; want %s", test.input, record.TimeZone, err, test.canonical)
		}
	}
}

func TestWeeklyISOWeekdaysAndStrictClock(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC) // Saturday
	record, err := NewWeekly("w", "Week", "Check", LocalTimeInput{
		Time: "09:00:00.5", TimeZone: "UTC", Weekdays: []int{5, 1},
	}, now)
	if err != nil || record.ScheduledAt != "2026-10-12T09:00:00.500Z" ||
		len(record.Weekdays) != 2 || record.Weekdays[0] != 1 || record.Weekdays[1] != 5 {
		t.Fatalf("weekly = %+v, %v", record, err)
	}
	if _, err := NewWeekly("w", "Week", "Check", LocalTimeInput{Time: "09:00:00", TimeZone: "UTC", Weekdays: []int{1, 1}}, now); err == nil {
		t.Fatal("duplicate weekday accepted")
	}
	if _, err := NewDaily("d", "Daily", "Check", LocalTimeInput{Time: "9:00:00", TimeZone: "UTC"}, now); err == nil {
		t.Fatal("noncanonical clock accepted")
	}
	if _, err := NewDaily("d", "Daily", "Check", LocalTimeInput{Time: "09:00:00", TimeZone: "Mars/Olympus"}, now); err == nil || !strings.Contains(err.Error(), "time_zone") {
		t.Fatalf("unknown zone error = %v", err)
	}
}
