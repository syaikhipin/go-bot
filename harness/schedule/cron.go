package schedule

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type cronField struct {
	values map[int]bool
	star   bool
}

type cronRule struct {
	minute, hour, monthDay, month, weekDay cronField
}

var cronNumber = regexp.MustCompile(`^\d+$`)
var cronRange = regexp.MustCompile(`^(\d+)-(\d+)(?:/(\d+))?$`)

const maxCronSafeInteger = 1<<53 - 1

func parseCronNumber(raw string) (int, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value > maxCronSafeInteger {
		return 0, errors.New("cron field number is not a safe integer")
	}
	return int(value), nil
}

func parseCronField(raw, label string, min, max int) (cronField, error) {
	if raw == "" {
		return cronField{}, fmt.Errorf("cron %s field is empty", label)
	}
	field := cronField{values: make(map[int]bool), star: strings.HasPrefix(raw, "*")}
	for _, item := range strings.Split(raw, ",") {
		start, end, step := 0, 0, 1
		switch {
		case item == "*":
			start, end = min, max
		case strings.HasPrefix(item, "*/") && cronNumber.MatchString(item[2:]):
			start, end = min, max
			var err error
			step, err = parseCronNumber(item[2:])
			if err != nil || step < 1 {
				return cronField{}, fmt.Errorf("cron %s field has an invalid step", label)
			}
		case cronNumber.MatchString(item):
			var err error
			start, err = parseCronNumber(item)
			if err != nil {
				return cronField{}, fmt.Errorf("cron %s field has an invalid value", label)
			}
			end = start
		case cronRange.MatchString(item):
			groups := cronRange.FindStringSubmatch(item)
			var err error
			start, err = parseCronNumber(groups[1])
			if err != nil {
				return cronField{}, fmt.Errorf("cron %s field has an invalid range start: %w", label, err)
			}
			end, err = parseCronNumber(groups[2])
			if err != nil {
				return cronField{}, fmt.Errorf("cron %s field has an invalid range end: %w", label, err)
			}
			if groups[3] != "" {
				step, err = parseCronNumber(groups[3])
				if err != nil {
					return cronField{}, fmt.Errorf("cron %s field has an invalid range step: %w", label, err)
				}
			}
		default:
			return cronField{}, fmt.Errorf("cron %s field element %q is unsupported", label, item)
		}
		if start < min || end > max || end < start || step < 1 {
			return cronField{}, fmt.Errorf("cron %s field element %q is out of range", label, item)
		}
		for value := start; value <= end; value += step {
			if label == "day-of-week" && value == 7 {
				value = 0
				field.values[value] = true
				break
			}
			field.values[value] = true
		}
	}
	if len(field.values) == 0 {
		return cronField{}, errors.New("cron field matches nothing")
	}
	return field, nil
}

func parseCron(expression string) (cronRule, string, error) {
	if expression == "" || strings.TrimSpace(expression) != expression {
		return cronRule{}, "", errors.New("cron.expression must be a non-empty trimmed string")
	}
	parts := strings.Fields(expression)
	if len(parts) != 5 {
		return cronRule{}, "", errors.New("cron.expression must contain exactly five fields")
	}
	specs := []struct {
		name     string
		min, max int
	}{
		{"minute", 0, 59}, {"hour", 0, 23}, {"day-of-month", 1, 31},
		{"month", 1, 12}, {"day-of-week", 0, 7},
	}
	fields := make([]cronField, 5)
	canonical := make([]string, 5)
	for index, spec := range specs {
		var err error
		fields[index], err = parseCronField(parts[index], spec.name, spec.min, spec.max)
		if err != nil {
			return cronRule{}, "", err
		}
		canonical[index] = encodeCronField(fields[index], spec.min, spec.max, index == 4)
	}
	return cronRule{fields[0], fields[1], fields[2], fields[3], fields[4]}, strings.Join(canonical, " "), nil
}

func encodeCronField(field cronField, min, max int, sundayFold bool) string {
	values := sortedCronValues(field)
	canonicalMax := max
	if sundayFold {
		canonicalMax = 6
	}
	if field.star {
		if len(values) == canonicalMax-min+1 {
			return "*"
		}
		bestStep, bestWalk := 0, []int{}
		for step := 1; step <= max-min+1; step++ {
			walked := map[int]bool{}
			for value := min; value <= max; value += step {
				if sundayFold && value == 7 {
					value = 0
					walked[value] = true
					break
				}
				walked[value] = true
			}
			allPresent := true
			for value := range walked {
				if !field.values[value] {
					allPresent = false
					break
				}
			}
			if allPresent && len(walked) > len(bestWalk) {
				bestStep = step
				bestWalk = bestWalk[:0]
				for value := range walked {
					bestWalk = append(bestWalk, value)
				}
			}
		}
		walked := map[int]bool{}
		for _, value := range bestWalk {
			walked[value] = true
		}
		remaining := make([]int, 0)
		for _, value := range values {
			if !walked[value] {
				remaining = append(remaining, value)
			}
		}
		if len(remaining) == 0 {
			return fmt.Sprintf("*/%d", bestStep)
		}
		return fmt.Sprintf("*/%d,%s", bestStep, encodeCronValues(remaining))
	}
	return encodeCronValues(values)
}

func encodeCronValues(values []int) string {
	if len(values) == 1 {
		return strconv.Itoa(values[0])
	}
	step := values[1] - values[0]
	uniform := true
	for index := 2; index < len(values); index++ {
		if values[index]-values[index-1] != step {
			uniform = false
			break
		}
	}
	if uniform {
		if step == 1 {
			return fmt.Sprintf("%d-%d", values[0], values[len(values)-1])
		}
		return fmt.Sprintf("%d-%d/%d", values[0], values[len(values)-1], step)
	}
	parts := make([]string, 0)
	start := values[0]
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1]+1 {
			continue
		}
		if start == values[index-1] {
			parts = append(parts, strconv.Itoa(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, values[index-1]))
		}
		start = values[index]
	}
	last := values[len(values)-1]
	if start == last {
		parts = append(parts, strconv.Itoa(last))
	} else {
		parts = append(parts, fmt.Sprintf("%d-%d", start, last))
	}
	return strings.Join(parts, ",")
}

func (rule cronRule) dayAllowed(date time.Time) bool {
	if !rule.month.values[int(date.Month())] {
		return false
	}
	monthDay := rule.monthDay.values[date.Day()]
	weekDay := rule.weekDay.values[int(date.Weekday())]
	if rule.monthDay.star || rule.weekDay.star {
		return monthDay && weekDay
	}
	return monthDay || weekDay
}

func nextCron(now time.Time, loc *time.Location, rule cronRule) (string, error) {
	local := now.In(loc)
	date := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, time.UTC)
	hours, minutes := sortedCronValues(rule.hour), sortedCronValues(rule.minute)
	for days := 0; days < 366*400; days++ {
		// A local date in year 10000 can still resolve to UTC year 9999
		// in positive-offset zones. canonicalUTC checks the real limit.
		if date.Year() > 10000 {
			break
		}
		if rule.dayAllowed(date) {
			for _, hour := range hours {
				for _, minute := range minutes {
					target, ok := wallInstant(date, wallClock{hour: hour, minute: minute}, loc)
					if ok && target.After(now) {
						return canonicalUTC(target)
					}
				}
			}
		}
		date = date.AddDate(0, 0, 1)
	}
	return "", errors.New("no future cron occurrence is representable")
}

func sortedCronValues(field cronField) []int {
	result := make([]int, 0, len(field.values))
	for value := range field.values {
		result = append(result, value)
	}
	sort.Ints(result)
	return result
}

func NewCron(id, title, prompt string, input CronInput, now time.Time) (Record, error) {
	record, err := newRecord(id, title, prompt, "cron")
	if err != nil {
		return Record{}, err
	}
	rule, expression, err := parseCron(input.Expression)
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
	record.Expression, record.TimeZone = expression, zoneName
	record.ScheduledAt, err = nextCron(now, loc, rule)
	return record, err
}
