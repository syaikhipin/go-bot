package schedule

import (
	"errors"
	"sort"
)

// EquivalentDailyRule validates a submitted selector and compares its
// normalized rule without searching for a new target. An unchanged rule may
// retain a committed target even when no further date is representable.
func EquivalentDailyRule(record Record, input LocalTimeInput) (bool, error) {
	_, clock, err := parseClock(input.Time)
	if err != nil {
		return false, err
	}
	_, zone, err := parseZone(input.TimeZone)
	if err != nil {
		return false, err
	}
	storedZone, err := CanonicalTimeZone(record.TimeZone)
	if err != nil {
		return false, err
	}
	return record.Kind == "daily" && record.Time == clock && storedZone == zone, nil
}

func normalizedWeekdays(days []int) ([]int, error) {
	if len(days) == 0 {
		return nil, errors.New("weekly requires weekdays")
	}
	seen := map[int]bool{}
	for _, day := range days {
		if day < 1 || day > 7 || seen[day] {
			return nil, errors.New("weekdays must be unique ISO weekday numbers 1-7")
		}
		seen[day] = true
	}
	result := append([]int(nil), days...)
	sort.Ints(result)
	return result, nil
}

// EquivalentWeeklyRule compares a normalized wall clock, IANA zone, and ISO
// weekday set before calculating any replacement target.
func EquivalentWeeklyRule(record Record, input LocalTimeInput) (bool, error) {
	_, clock, err := parseClock(input.Time)
	if err != nil {
		return false, err
	}
	_, zone, err := parseZone(input.TimeZone)
	if err != nil {
		return false, err
	}
	weekdays, err := normalizedWeekdays(input.Weekdays)
	if err != nil {
		return false, err
	}
	storedZone, err := CanonicalTimeZone(record.TimeZone)
	if err != nil {
		return false, err
	}
	if record.Kind != "weekly" || record.Time != clock || storedZone != zone || len(record.Weekdays) != len(weekdays) {
		return false, nil
	}
	for index, day := range weekdays {
		if record.Weekdays[index] != day {
			return false, nil
		}
	}
	return true, nil
}

// EquivalentCronRule compares canonical expression and zone without walking
// an unsatisfiable expression's future search horizon.
func EquivalentCronRule(record Record, input CronInput) (bool, error) {
	_, expression, err := parseCron(input.Expression)
	if err != nil {
		return false, err
	}
	_, zone, err := parseZone(input.TimeZone)
	if err != nil {
		return false, err
	}
	storedZone, err := CanonicalTimeZone(record.TimeZone)
	if err != nil {
		return false, err
	}
	return record.Kind == "cron" && record.Expression == expression && storedZone == zone, nil
}
