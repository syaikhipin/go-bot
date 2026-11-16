package schedule

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

const rruleDateTime = "20060102T150405"

var (
	basicStart  = regexp.MustCompile(`^DTSTART(?::([0-9]{8}T[0-9]{6})Z|;TZID=([^:;]+):([0-9]{8}T[0-9]{6}))$`)
	basicUntil  = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)
	weekdayRule = regexp.MustCompile(`^([+-]?[1-9][0-9]?)?(MO|TU|WE|TH|FR|SA|SU)$`)
	integerRule = regexp.MustCompile(`^-?[0-9]+$`)
)

type recurrenceRule struct {
	expression string
	location   *time.Location
	options    rrule.ROption
	count      int
	until      time.Time
}

func NewRRule(id, title, prompt, expression string, now time.Time) (Record, error) {
	rule, err := parseRRule(expression, true)
	if err != nil {
		return Record{}, err
	}
	record, err := newRecord(id, title, prompt, "rrule")
	if err != nil {
		return Record{}, err
	}
	_, next, err := rule.window(now, now)
	if err != nil {
		return Record{}, err
	}
	if next.IsZero() {
		return Record{}, errors.New("RRULE has no future occurrence")
	}
	record.RRule = rule.expression
	record.ScheduledAt, err = canonicalUTC(next)
	return record, err
}

func parseRRule(expression string, authoring bool) (recurrenceRule, error) {
	lines := strings.FieldsFunc(expression, func(r rune) bool { return r == '\n' })
	parts := lines[:0]
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	if len(parts) != 2 || !strings.HasPrefix(parts[1], "RRULE:") {
		return recurrenceRule{}, errors.New("RRULE requires one DTSTART line and one RRULE line")
	}
	match := basicStart.FindStringSubmatch(parts[0])
	if match == nil {
		return recurrenceRule{}, errors.New("DTSTART requires UTC or an explicit IANA TZID")
	}
	loc, text := time.UTC, match[1]
	if match[2] != "" {
		var zone string
		var err error
		loc, zone, err = parseZone(match[2])
		if err != nil {
			return recurrenceRule{}, err
		}
		text = match[3]
		if zone == "UTC" {
			parts[0] = "DTSTART:" + text + "Z"
		} else {
			parts[0] = "DTSTART;TZID=" + zone + ":" + text
		}
	}
	start, err := time.Parse(rruleDateTime, text)
	if err != nil || start.Year() < 100 {
		return recurrenceRule{}, errors.New("invalid RRULE DTSTART")
	}
	if _, ok := recurrenceInstant(start, loc); !ok {
		return recurrenceRule{}, errors.New("RRULE DTSTART does not exist in its time zone")
	}
	fields, err := recurrenceParts(strings.TrimPrefix(parts[1], "RRULE:"))
	if err != nil {
		return recurrenceRule{}, err
	}
	opt, err := rrule.StrToROption(parts[1])
	if err != nil {
		return recurrenceRule{}, err
	}
	opt.Dtstart = start
	if opt.Interval == 0 {
		opt.Interval = 1
	}
	count, until := opt.Count, opt.Until
	if count > 1000 || (count != 0 && !until.IsZero()) || (!until.IsZero() && until.Year() < 100) {
		return recurrenceRule{}, errors.New("RRULE supports COUNT from 1 to 1000 or a UTC UNTIL")
	}
	if until.IsZero() {
		until = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	}
	opt.Count = 0
	opt.Until = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	if err := validateRecurrenceOptions(opt, fields, authoring); err != nil {
		return recurrenceRule{}, err
	}
	// A larger interval can never reach a second occurrence in the supported
	// date range. Bound it before handing calendar arithmetic to the library.
	opt.Interval = min(opt.Interval, 6_000_000_000)
	return recurrenceRule{expression: strings.Join(parts, "\n"), location: loc, options: *opt, count: count, until: until}, nil
}

func recurrenceParts(line string) (map[string]string, error) {
	fields := make(map[string]string)
	for field := range strings.SplitSeq(line, ";") {
		key, value, ok := strings.Cut(field, "=")
		if !ok || value == "" || fields[key] != "" {
			return nil, errors.New("RRULE fields must be nonempty and unique")
		}
		switch key {
		case "FREQ":
		case "INTERVAL", "COUNT":
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil || !integerRule.MatchString(value) || number < 1 || number > 9_007_199_254_740_991 {
				return nil, fmt.Errorf("invalid RRULE %s", key)
			}
		case "UNTIL":
			if !basicUntil.MatchString(value) {
				return nil, errors.New("RRULE UNTIL must be a UTC date-time")
			}
		case "WKST":
			if !weekdayRule.MatchString(value) || len(value) != 2 {
				return nil, errors.New("invalid RRULE WKST")
			}
		case "BYDAY", "BYSECOND", "BYMINUTE", "BYHOUR", "BYMONTHDAY", "BYYEARDAY", "BYWEEKNO", "BYMONTH", "BYSETPOS":
			seen := make(map[string]bool)
			for item := range strings.SplitSeq(value, ",") {
				canonical := item
				if key == "BYDAY" {
					match := weekdayRule.FindStringSubmatch(item)
					if match == nil {
						return nil, errors.New("invalid RRULE BYDAY")
					}
					ordinal, _ := strconv.Atoi(match[1])
					canonical = strconv.Itoa(ordinal) + match[2]
				} else {
					number, err := strconv.Atoi(item)
					if err != nil || !integerRule.MatchString(item) {
						return nil, fmt.Errorf("invalid RRULE %s", key)
					}
					canonical = strconv.Itoa(number)
				}
				if seen[canonical] {
					return nil, fmt.Errorf("duplicate RRULE %s value", key)
				}
				seen[canonical] = true
			}
		default:
			return nil, fmt.Errorf("unsupported RRULE field %q", key)
		}
		fields[key] = value
	}
	return fields, nil
}

func validateRecurrenceOptions(opt *rrule.ROption, fields map[string]string, authoring bool) error {
	if opt.Freq == rrule.SECONDLY || len(opt.Bysecond) > 1 ||
		((opt.Freq == rrule.MINUTELY || opt.Freq == rrule.HOURLY) && hasRecurrenceByParts(fields)) {
		return errors.New("RRULE violates minimum occurrence spacing")
	}
	if opt.Freq != rrule.YEARLY && (len(opt.Byyearday) > 0 || len(opt.Byweekno) > 0) {
		return errors.New("RRULE year or week numbers require YEARLY")
	}
	if opt.Freq != rrule.YEARLY && opt.Freq != rrule.MONTHLY && len(opt.Bymonthday) > 0 {
		return errors.New("RRULE month days require MONTHLY or YEARLY")
	}
	ordinary, ordinals := 0, 0
	for _, weekday := range opt.Byweekday {
		if weekday.N() == 0 {
			ordinary++
		} else {
			ordinals++
		}
	}
	if ordinals > 0 && (opt.Freq != rrule.YEARLY && opt.Freq != rrule.MONTHLY || len(opt.Byweekno) > 0) {
		return errors.New("invalid RRULE weekday ordinals")
	}
	if len(opt.Bysetpos) > 0 {
		by := 0
		for key := range fields {
			if strings.HasPrefix(key, "BY") {
				by++
			}
		}
		if by < 2 {
			return errors.New("RRULE BYSETPOS requires another BY field")
		}
	}
	applyRecurrenceDefaults(opt)
	size := func(list []int, fallback int) int {
		if len(list) == 0 {
			return fallback
		}
		return len(list)
	}
	weekdayDays := func(perWeek int) int {
		if len(opt.Byweekday) == 0 {
			return 366
		}
		return ordinals + perWeek*ordinary
	}
	days := 1
	switch opt.Freq {
	case rrule.WEEKLY:
		days = min(7, weekdayDays(1))
	case rrule.MONTHLY:
		days = min(31, size(opt.Bymonthday, 31), weekdayDays(5))
	case rrule.YEARLY:
		months := size(opt.Bymonth, 12)
		weekdayBound := weekdayDays(53)
		if len(opt.Bymonth) > 0 {
			weekdayBound = months * weekdayDays(5)
		}
		days = min(366, 31*months, size(opt.Byyearday, 366), months*size(opt.Bymonthday, 31),
			9*size(opt.Byweekno, 53), weekdayBound)
	}
	candidates := days * size(opt.Byhour, 1) * size(opt.Byminute, 1) * size(opt.Bysecond, 1)
	if candidates > 10_080 || len(opt.Bysetpos) > 31 || authoring && candidates == 1 && len(opt.Bysetpos) > 0 {
		return errors.New("RRULE exceeds its occurrence or set-position bound")
	}
	for _, position := range opt.Bysetpos {
		if position < -candidates || position > candidates {
			return errors.New("RRULE set position is outside its recurrence period")
		}
	}
	_, err := rrule.NewRRule(*opt)
	return err
}

func hasRecurrenceByParts(fields map[string]string) bool {
	for field := range fields {
		if strings.HasPrefix(field, "BY") {
			return true
		}
	}
	return false
}

func applyRecurrenceDefaults(opt *rrule.ROption) {
	if opt.Freq >= rrule.HOURLY {
		return
	}
	start := opt.Dtstart
	if len(opt.Byhour) == 0 {
		opt.Byhour = []int{start.Hour()}
	}
	if len(opt.Byminute) == 0 {
		opt.Byminute = []int{start.Minute()}
	}
	if len(opt.Bysecond) == 0 {
		opt.Bysecond = []int{start.Second()}
	}
	if len(opt.Byweekday)+len(opt.Bymonthday)+len(opt.Byyearday)+len(opt.Byweekno) != 0 {
		return
	}
	switch opt.Freq {
	case rrule.WEEKLY:
		weekdays := [...]rrule.Weekday{rrule.SU, rrule.MO, rrule.TU, rrule.WE, rrule.TH, rrule.FR, rrule.SA}
		opt.Byweekday = []rrule.Weekday{weekdays[start.Weekday()]}
	case rrule.MONTHLY, rrule.YEARLY:
		opt.Bymonthday = []int{start.Day()}
		if opt.Freq == rrule.YEARLY && len(opt.Bymonth) == 0 {
			opt.Bymonth = []int{int(start.Month())}
		}
	}
}

func recurrenceInstant(wall time.Time, location *time.Location) (time.Time, bool) {
	if location == time.UTC {
		return wall, true
	}
	return wallInstant(wall, wallClock{hour: wall.Hour(), minute: wall.Minute(), second: wall.Second()}, location)
}

func (r recurrenceRule) wall(value time.Time) time.Time {
	local := value.In(r.location)
	return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), 0, time.UTC)
}

func (r recurrenceRule) window(floor, decision time.Time) (time.Time, time.Time, error) {
	if r.count > 0 {
		return r.scan(r.options.Dtstart, decision, r.count)
	}
	bound := r.wall(decision)
	if decision.After(r.until) {
		bound = r.wall(r.until)
	}
	saved := r.wall(floor)
	for span := int64(2 * 86400); ; span *= 2 {
		from := time.Unix(max(bound.Unix()-span, saved.Unix()), 0).UTC()
		latest, next, err := r.scan(from, decision, 0)
		if err != nil || !latest.IsZero() || !from.After(saved) {
			return latest, next, err
		}
	}
}

func (r recurrenceRule) scan(from, decision time.Time, count int) (latest, next time.Time, err error) {
	opt := r.options
	if count == 0 {
		opt.Dtstart = recurrenceAnchor(opt, from)
	}
	recurrence, err := rrule.NewRRule(opt)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	iterate := recurrence.Iterator()
	for wall, ok := iterate(); ok; wall, ok = iterate() {
		if wall.Before(from) {
			continue
		}
		if wall.Unix() > r.until.Unix()+86400 {
			break
		}
		instant, valid := recurrenceInstant(wall, r.location)
		if !valid || instant.After(r.until) || instant.Equal(latest) {
			continue
		}
		if instant.After(decision) {
			next = instant
			break
		}
		latest = instant
		if count > 0 {
			count--
			if count == 0 {
				break
			}
		}
	}
	return latest, next, nil
}

func recurrenceAnchor(opt rrule.ROption, from time.Time) time.Time {
	start := opt.Dtstart
	if !from.After(start) {
		return start
	}
	interval := int64(opt.Interval)
	unit := int64(86400)
	switch opt.Freq {
	case rrule.MINUTELY, rrule.HOURLY, rrule.DAILY, rrule.WEEKLY:
		switch opt.Freq {
		case rrule.MINUTELY:
			unit = 60
		case rrule.HOURLY:
			unit = 3600
		case rrule.WEEKLY:
			unit *= 7
		}
		// Back up one aligned period to retain BY times before the rebased
		// DTSTART clock (and weekdays before the DTSTART weekday).
		period := unit * interval
		steps := max(0, (from.Unix()-start.Unix())/period-1)
		return time.Unix(start.Unix()+steps*period, 0).UTC()
	case rrule.MONTHLY:
		months := int64((from.Year()-start.Year())*12 + int(from.Month()-start.Month()))
		offset := max(0, months/interval-1) * interval
		if offset > 0 {
			return time.Date(start.Year(), start.Month()+time.Month(offset), 1, 0, 0, 0, 0, time.UTC)
		}
	case rrule.YEARLY:
		years := int64(from.Year() - start.Year())
		offset := max(0, years/interval-1) * interval
		if offset > 0 {
			return time.Date(start.Year()+int(offset), 1, 1, 0, 0, 0, 0, time.UTC)
		}
	}
	return start
}

// EquivalentRRule checks the stored normalized rule without advancing it.
func EquivalentRRule(previous Record, expression string) (bool, error) {
	if strings.TrimSpace(expression) == previous.RRule {
		return true, nil
	}
	rule, err := parseRRule(expression, true)
	return rule.expression == previous.RRule, err
}
