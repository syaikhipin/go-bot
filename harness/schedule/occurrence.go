package schedule

import (
	"errors"
	"fmt"
	"time"
)

// Occurrence is the latest due event for one recurring rule and its next
// committed target. An empty NextScheduledAt means the four-digit calendar is
// exhausted and the task becomes inactive after delivery.
type Occurrence struct {
	OccurrenceAt    string
	NextScheduledAt string
}

func dueTarget(record Record, acceptedAt time.Time) (time.Time, error) {
	target, err := time.Parse("2006-01-02T15:04:05.000Z", record.ScheduledAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid committed scheduledAt: %w", err)
	}
	if acceptedAt.Before(target) {
		return time.Time{}, errors.New("Schedule dispatch precedes its committed target")
	}
	if acceptedAt.UTC().Year() < 1 || acceptedAt.UTC().Year() > 9999 {
		return time.Time{}, errors.New("Schedule decision is outside four-digit UTC years")
	}
	return target, nil
}

func latestWall(now, floor time.Time, loc *time.Location, clocks []wallClock, dayAllowed func(time.Time) bool) time.Time {
	utc := now.UTC()
	date := time.Date(utc.Year(), utc.Month(), utc.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	for days := 0; days < 366*400; days++ {
		if dayAllowed(date) {
			for index := len(clocks) - 1; index >= 0; index-- {
				candidate, ok := wallInstant(date, clocks[index], loc)
				if ok && !candidate.After(now) {
					if candidate.Before(floor) {
						return floor
					}
					return candidate
				}
			}
		}
		date = date.AddDate(0, 0, -1)
	}
	return floor
}

func ResolveRecurringOccurrence(record Record, acceptedAt time.Time) (Occurrence, error) {
	target, err := dueTarget(record, acceptedAt)
	if err != nil {
		return Occurrence{}, err
	}
	switch record.Kind {
	case "rrule":
		rule, err := parseRRule(record.RRule, false)
		if err != nil {
			return Occurrence{}, err
		}
		latest, next, err := rule.window(target, acceptedAt)
		if err != nil {
			return Occurrence{}, err
		}
		if latest.Before(target) {
			latest = target
		}
		occurrence, err := canonicalUTC(latest)
		if err != nil {
			return Occurrence{}, err
		}
		result := Occurrence{OccurrenceAt: occurrence}
		if !next.IsZero() {
			result.NextScheduledAt, err = canonicalUTC(next)
		}
		return result, err
	case "every":
		if record.EverySeconds < 60 {
			return Occurrence{}, errors.New("invalid stored every interval")
		}
		interval := record.EverySeconds * 1000
		steps := (acceptedAt.UnixMilli() - target.UnixMilli()) / interval
		occurrence := time.UnixMilli(target.UnixMilli() + steps*interval)
		result := Occurrence{}
		result.OccurrenceAt, err = canonicalUTC(occurrence)
		if err != nil {
			return Occurrence{}, err
		}
		result.NextScheduledAt, _ = canonicalUTC(time.UnixMilli(occurrence.UnixMilli() + interval))
		return result, nil
	case "daily", "weekly":
		loc, _, err := parseZone(record.TimeZone)
		if err != nil {
			return Occurrence{}, err
		}
		clock, _, err := parseClock(record.Time)
		if err != nil {
			return Occurrence{}, err
		}
		allowed := func(time.Time) bool { return true }
		if record.Kind == "weekly" {
			if len(record.Weekdays) == 0 {
				return Occurrence{}, errors.New("stored weekly rule has no weekdays")
			}
			set := map[int]bool{}
			for _, day := range record.Weekdays {
				set[day] = true
			}
			allowed = func(date time.Time) bool {
				day := int(date.Weekday())
				if day == 0 {
					day = 7
				}
				return set[day]
			}
		}
		latest := latestWall(acceptedAt, target, loc, []wallClock{clock}, allowed)
		result := Occurrence{}
		result.OccurrenceAt, err = canonicalUTC(latest)
		if err != nil {
			return Occurrence{}, err
		}
		// The selected local date is consumed even across a clock rollback.
		localDate := latest.In(loc)
		cursor := time.Date(localDate.Year(), localDate.Month(), localDate.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
		// The local calendar can reach year 10000 while its UTC instant is
		// still in 9999; canonicalUTC validates the committed target.
		for days := 0; days < 366*400 && cursor.Year() <= 10000; days++ {
			if allowed(cursor) {
				if next, ok := wallInstant(cursor, clock, loc); ok && next.After(acceptedAt) {
					result.NextScheduledAt, _ = canonicalUTC(next)
					break
				}
			}
			cursor = cursor.AddDate(0, 0, 1)
		}
		return result, nil
	case "cron":
		rule, _, err := parseCron(record.Expression)
		if err != nil {
			return Occurrence{}, err
		}
		loc, _, err := parseZone(record.TimeZone)
		if err != nil {
			return Occurrence{}, err
		}
		var clocks []wallClock
		for _, hour := range sortedCronValues(rule.hour) {
			for _, minute := range sortedCronValues(rule.minute) {
				clocks = append(clocks, wallClock{hour: hour, minute: minute})
			}
		}
		latest := latestWall(acceptedAt, target, loc, clocks, rule.dayAllowed)
		result := Occurrence{}
		result.OccurrenceAt, err = canonicalUTC(latest)
		if err != nil {
			return Occurrence{}, err
		}
		result.NextScheduledAt, _ = nextCron(acceptedAt, loc, rule)
		return result, nil
	default:
		return Occurrence{}, errors.New("Schedule record is not recurring")
	}
}
