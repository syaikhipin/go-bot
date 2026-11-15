package schedule

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Record is the Session-bound Schedule rule. The task's delivery status and
// receipts live beside it, never in the DeepSeek v4 Session event stream.
type Record struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	Prompt       string `json:"prompt"`
	AfterSeconds int64  `json:"afterSeconds,omitempty"`
	EverySeconds int64  `json:"everySeconds,omitempty"`
	Time         string `json:"time,omitempty"`
	TimeZone     string `json:"timeZone,omitempty"`
	Weekdays     []int  `json:"weekdays,omitempty"`
	Expression   string `json:"expression,omitempty"`
	RRule        string `json:"rrule,omitempty"`
	ScheduledAt  string `json:"scheduledAt"`
}

type LocalTimeInput struct {
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
	Weekdays []int  `json:"weekdays,omitempty"`
}

type CronInput struct {
	Expression string `json:"expression"`
	TimeZone   string `json:"time_zone"`
}

type LocalAtInput struct {
	Date     string `json:"date"`
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
}

var clockPattern = regexp.MustCompile(`^(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?$`)
var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
var offsetInstantPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?(?:Z|[+-]\d{2}:\d{2})$`)

func newRecord(id, title, prompt, kind string) (Record, error) {
	title, prompt = strings.TrimSpace(title), strings.TrimSpace(prompt)
	if id == "" || title == "" || utf8.RuneCountInString(title) > 120 || prompt == "" {
		return Record{}, errors.New("Schedule requires an ID, a nonempty title of at most 120 characters, and a nonempty prompt")
	}
	return Record{ID: id, Kind: kind, Title: title, Prompt: prompt}, nil
}

func canonicalUTC(instant time.Time) (string, error) {
	instant = instant.UTC().Truncate(time.Millisecond)
	if instant.Year() < 1 || instant.Year() > 9999 {
		return "", errors.New("Schedule target is outside four-digit UTC years")
	}
	return instant.Format("2006-01-02T15:04:05.000Z"), nil
}

func validateCalendarAcceptance(now time.Time) error {
	year := now.UTC().Year()
	if year < 1 || year > 9999 {
		return errors.New("Schedule creation time is outside four-digit UTC years")
	}
	return nil
}

func positiveFuture(now, target time.Time) (string, error) {
	if !target.After(now) {
		return "", errors.New("Schedule target must be in the future")
	}
	return canonicalUTC(target)
}

func NewAfter(id, title, prompt string, seconds int64, now time.Time) (Record, error) {
	record, err := newRecord(id, title, prompt, "after")
	if err != nil {
		return Record{}, err
	}
	if seconds < 1 || seconds > 253402300799 {
		return Record{}, errors.New("after_seconds must be a positive safe interval")
	}
	record.ScheduledAt, err = positiveFuture(now, time.Unix(now.Unix()+seconds, int64(now.Nanosecond())))
	if err != nil {
		return Record{}, err
	}
	record.AfterSeconds = seconds
	return record, nil
}

func NewAt(id, title, prompt, at string, now time.Time) (Record, error) {
	record, err := newRecord(id, title, prompt, "at")
	if err != nil {
		return Record{}, err
	}
	record.ScheduledAt, err = NormalizeAt(at)
	if err != nil {
		return Record{}, err
	}
	target, _ := time.Parse("2006-01-02T15:04:05.000Z", record.ScheduledAt)
	record.ScheduledAt, err = positiveFuture(now, target)
	return record, err
}

// NormalizeAt parses an absolute target without requiring that it still be in
// the future. An equivalent update may retain an already-due committed target.
func NormalizeAt(at string) (string, error) {
	if !offsetInstantPattern.MatchString(at) {
		return "", errors.New("at requires a strict instant with an explicit UTC offset")
	}
	if !strings.HasSuffix(at, "Z") {
		offset := at[len(at)-6:]
		hour, _ := strconv.Atoi(offset[1:3])
		minute, _ := strconv.Atoi(offset[4:6])
		if hour > 23 || minute > 59 || offset == "-00:00" {
			return "", errors.New("at numeric UTC offset is invalid")
		}
	}
	target, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return "", err
	}
	return canonicalUTC(target)
}

func NewLocalAt(id, title, prompt string, input LocalAtInput, now time.Time) (Record, error) {
	record, err := newRecord(id, title, prompt, "at")
	if err != nil {
		return Record{}, err
	}
	record.ScheduledAt, err = NormalizeLocalAt(input)
	if err != nil {
		return Record{}, err
	}
	target, _ := time.Parse("2006-01-02T15:04:05.000Z", record.ScheduledAt)
	record.ScheduledAt, err = positiveFuture(now, target)
	return record, err
}

// NormalizeLocalAt resolves a local calendar target independent of whether it
// is still future relative to the update's acceptance time.
func NormalizeLocalAt(input LocalAtInput) (string, error) {
	if !datePattern.MatchString(input.Date) {
		return "", errors.New("at.date must be YYYY-MM-DD")
	}
	date, err := time.Parse("2006-01-02", input.Date)
	if err != nil {
		return "", err
	}
	clock, _, err := parseClock(input.Time)
	if err != nil {
		return "", err
	}
	loc, _, err := parseZone(input.TimeZone)
	if err != nil {
		return "", err
	}
	target, ok := wallInstant(date, clock, loc)
	if !ok {
		return "", errors.New("local at time does not exist in its time zone")
	}
	return canonicalUTC(target)
}

func NewEvery(id, title, prompt string, seconds int64, now time.Time) (Record, error) {
	record, err := newRecord(id, title, prompt, "every")
	if err != nil {
		return Record{}, err
	}
	if seconds < 60 || seconds > 253402300799 {
		return Record{}, errors.New("every_seconds must be at least 60 and representable")
	}
	record.ScheduledAt, err = positiveFuture(now, time.Unix(now.Unix()+seconds, int64(now.Nanosecond())))
	if err != nil {
		return Record{}, err
	}
	record.EverySeconds = seconds
	return record, nil
}

type wallClock struct{ hour, minute, second, millisecond int }

func parseClock(value string) (wallClock, string, error) {
	groups := clockPattern.FindStringSubmatch(value)
	if groups == nil {
		return wallClock{}, "", errors.New("time must be HH:mm:ss with optional 1-3 millisecond digits")
	}
	parse := func(s string) int { v, _ := strconv.Atoi(s); return v }
	clock := wallClock{hour: parse(groups[1]), minute: parse(groups[2]), second: parse(groups[3])}
	if clock.hour > 23 || clock.minute > 59 || clock.second > 59 {
		return wallClock{}, "", errors.New("invalid local clock time")
	}
	if groups[4] != "" {
		clock.millisecond = parse(groups[4] + strings.Repeat("0", 3-len(groups[4])))
	}
	return clock, fmt.Sprintf("%02d:%02d:%02d.%03d", clock.hour, clock.minute, clock.second, clock.millisecond), nil
}

// wallInstant returns the earlier matching instant during an overlap and no
// instant for a skipped local time. time.Date alone normalizes a DST gap and
// cannot be used as proof that the requested wall clock existed.
func wallInstant(date time.Time, clock wallClock, loc *time.Location) (time.Time, bool) {
	naive := time.Date(date.Year(), date.Month(), date.Day(), clock.hour, clock.minute, clock.second, clock.millisecond*1e6, time.UTC)
	var best time.Time
	seen := map[int]bool{}
	for h := -48; h <= 48; h += 3 {
		_, offset := naive.Add(time.Duration(h) * time.Hour).In(loc).Zone()
		if seen[offset] {
			continue
		}
		seen[offset] = true
		candidate := naive.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(loc)
		if local.Year() != date.Year() || local.Month() != date.Month() || local.Day() != date.Day() ||
			local.Hour() != clock.hour || local.Minute() != clock.minute || local.Second() != clock.second ||
			local.Nanosecond()/1e6 != clock.millisecond {
			continue
		}
		if best.IsZero() || candidate.Before(best) {
			best = candidate
		}
	}
	return best, !best.IsZero()
}

func nextCalendar(now time.Time, loc *time.Location, clock wallClock, dayAllowed func(time.Time) bool) (string, error) {
	local := now.In(loc)
	date := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, time.UTC)
	for days := 0; days < 366*400; days++ {
		// Local year 10000 may still map to a supported UTC instant in
		// positive-offset zones. canonicalUTC enforces the actual boundary.
		if date.Year() > 10000 {
			break
		}
		if dayAllowed(date) {
			if target, ok := wallInstant(date, clock, loc); ok && target.After(now) {
				return canonicalUTC(target)
			}
		}
		date = date.AddDate(0, 0, 1)
	}
	return "", errors.New("no future wall-clock occurrence is representable")
}

func NewDaily(id, title, prompt string, input LocalTimeInput, now time.Time) (Record, error) {
	record, err := newRecord(id, title, prompt, "daily")
	if err != nil {
		return Record{}, err
	}
	clock, normalized, err := parseClock(input.Time)
	if err != nil {
		return Record{}, err
	}
	loc, zoneName, err := parseZone(input.TimeZone)
	if err != nil {
		return Record{}, err
	}
	if err := validateCalendarAcceptance(now); err != nil {
		return Record{}, err
	}
	record.Time, record.TimeZone = normalized, zoneName
	record.ScheduledAt, err = nextCalendar(now, loc, clock, func(time.Time) bool { return true })
	return record, err
}

func NewWeekly(id, title, prompt string, input LocalTimeInput, now time.Time) (Record, error) {
	record, err := newRecord(id, title, prompt, "weekly")
	if err != nil {
		return Record{}, err
	}
	clock, normalized, err := parseClock(input.Time)
	if err != nil {
		return Record{}, err
	}
	loc, zoneName, err := parseZone(input.TimeZone)
	if err != nil {
		return Record{}, err
	}
	record.Weekdays, err = normalizedWeekdays(input.Weekdays)
	if err != nil {
		return Record{}, err
	}
	if err := validateCalendarAcceptance(now); err != nil {
		return Record{}, err
	}
	set := map[int]bool{}
	for _, day := range record.Weekdays {
		set[day] = true
	}
	record.Time, record.TimeZone = normalized, zoneName
	record.ScheduledAt, err = nextCalendar(now, loc, clock, func(date time.Time) bool {
		day := int(date.Weekday())
		if day == 0 {
			day = 7
		}
		return set[day]
	})
	return record, err
}
