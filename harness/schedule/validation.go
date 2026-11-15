package schedule

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

func validateInstant(value string) error {
	instant, err := time.Parse("2006-01-02T15:04:05.000Z", value)
	if err != nil {
		return err
	}
	canonical, err := canonicalUTC(instant)
	if err != nil {
		return err
	}
	if canonical != value {
		return errors.New("instant is not canonical UTC milliseconds")
	}
	return nil
}

func validateStoredRecord(record Record) error {
	common, err := newRecord(record.ID, record.Title, record.Prompt, record.Kind)
	if err != nil {
		return err
	}
	if common.Title != record.Title || common.Prompt != record.Prompt {
		return errors.New("stored Schedule title and prompt must be trimmed")
	}
	if err := validateInstant(record.ScheduledAt); err != nil {
		return fmt.Errorf("scheduledAt: %w", err)
	}
	selectorEmpty := record.AfterSeconds == 0 && record.EverySeconds == 0 && record.Time == "" &&
		record.TimeZone == "" && len(record.Weekdays) == 0 && record.Expression == ""
	if record.Kind != "rrule" && record.RRule != "" {
		return errors.New("unexpected stored RRULE")
	}
	switch record.Kind {
	case "rrule":
		rule, err := parseRRule(record.RRule, false)
		if err != nil || rule.expression != record.RRule || record.TimeZone != "" ||
			record.AfterSeconds != 0 || record.EverySeconds != 0 || record.Time != "" || len(record.Weekdays) != 0 || record.Expression != "" {
			return errors.New("invalid stored RRULE")
		}
	case "after":
		if record.AfterSeconds < 1 || record.EverySeconds != 0 || record.Time != "" || record.TimeZone != "" || len(record.Weekdays) != 0 || record.Expression != "" {
			return errors.New("invalid stored after rule")
		}
	case "at":
		if !selectorEmpty {
			return errors.New("invalid stored at rule")
		}
	case "every":
		if record.EverySeconds < 60 || record.AfterSeconds != 0 || record.Time != "" || record.TimeZone != "" || len(record.Weekdays) != 0 || record.Expression != "" {
			return errors.New("invalid stored every rule")
		}
	case "daily", "weekly":
		_, normalized, err := parseClock(record.Time)
		if err != nil || normalized != record.Time {
			return errors.New("invalid stored wall clock")
		}
		_, _, err = parseZone(record.TimeZone)
		if err != nil {
			return errors.New("invalid stored IANA time zone")
		}
		if record.AfterSeconds != 0 || record.EverySeconds != 0 || record.Expression != "" {
			return errors.New("unexpected stored wall-clock selector")
		}
		if record.Kind == "daily" && len(record.Weekdays) != 0 {
			return errors.New("daily rule cannot contain weekdays")
		}
		if record.Kind == "weekly" {
			if len(record.Weekdays) == 0 {
				return errors.New("weekly rule requires weekdays")
			}
			previous := 0
			for _, day := range record.Weekdays {
				if day < 1 || day > 7 || day <= previous {
					return errors.New("weekly weekdays must be unique ascending ISO values")
				}
				previous = day
			}
		}
	case "cron":
		_, normalized, err := parseCron(record.Expression)
		if err != nil || normalized != record.Expression {
			return errors.New("invalid stored cron expression")
		}
		_, _, err = parseZone(record.TimeZone)
		if err != nil {
			return errors.New("invalid stored cron time zone")
		}
		if record.AfterSeconds != 0 || record.EverySeconds != 0 || record.Time != "" || len(record.Weekdays) != 0 {
			return errors.New("unexpected stored cron selector")
		}
	default:
		return errors.New("unknown Schedule kind")
	}
	return nil
}

func validateDocument(document Document) error {
	if document.Version != 1 {
		return errors.New("unsupported Schedule document version")
	}
	ids := make(map[string]bool, len(document.Tasks))
	for _, task := range document.Tasks {
		if err := validateStoredRecord(task.Record); err != nil {
			return fmt.Errorf("Schedule %q: %w", task.Record.ID, err)
		}
		if ids[task.Record.ID] {
			return fmt.Errorf("duplicate Schedule ID %q", task.Record.ID)
		}
		ids[task.Record.ID] = true
		if !taskStatusValid(task.Status) {
			return errors.New("invalid Schedule task status")
		}
		if task.Target != nil {
			if err := validateTarget(*task.Target); err != nil {
				return fmt.Errorf("Schedule %q: %w", task.Record.ID, err)
			}
		}
		if task.Target != nil && task.Target.Kind == "new" {
			if task.LastDelivery != nil && task.LastDelivery.TargetSessionID == "" {
				return errors.New("Schedule receipt lacks its target Session")
			}
			for _, receipt := range task.DeliveryHistory.Records {
				if receipt.TargetSessionID == "" {
					return errors.New("Schedule receipt lacks its target Session")
				}
			}
		}
		if task.PendingDelivery != nil && (task.Target == nil || task.Target.Kind != "new" || uuid.Validate(task.PendingDelivery.TargetSessionID) != nil) {
			return errors.New("invalid pending Schedule delivery")
		}
		for _, instant := range []string{task.CreatedAt, task.UpdatedAt} {
			if instant != "" && validateInstant(instant) != nil {
				return errors.New("invalid Schedule task timestamp")
			}
		}
		seen := map[string]bool{}
		for _, receipt := range task.DeliveryHistory.Records {
			if receipt.MessageID == "" || strings.TrimSpace(receipt.MessageID) != receipt.MessageID || seen[receipt.MessageID] ||
				validateInstant(receipt.ScheduledAt) != nil || validateInstant(receipt.DeliveredAt) != nil {
				return errors.New("invalid Schedule delivery history")
			}
			seen[receipt.MessageID] = true
		}
		if last := task.LastDelivery; last != nil {
			if last.MessageID == "" || strings.TrimSpace(last.MessageID) != last.MessageID || last.Prompt != "" ||
				validateInstant(last.ScheduledAt) != nil || validateInstant(last.DeliveredAt) != nil {
				return errors.New("invalid Schedule last delivery")
			}
		}
		if task.DeliveryHistory.Records != nil && len(task.DeliveryHistory.Records) == 0 && task.LastDelivery != nil {
			return errors.New("Schedule known-empty history conflicts with last delivery")
		}
		if count := len(task.DeliveryHistory.Records); count > 0 {
			latest := task.DeliveryHistory.Records[count-1]
			if task.LastDelivery == nil || task.LastDelivery.MessageID != latest.MessageID ||
				task.LastDelivery.ScheduledAt != latest.ScheduledAt || task.LastDelivery.DeliveredAt != latest.DeliveredAt ||
				task.LastDelivery.TargetSessionID != latest.TargetSessionID {
				return errors.New("Schedule last delivery differs from its saved history")
			}
		}
	}
	return nil
}
