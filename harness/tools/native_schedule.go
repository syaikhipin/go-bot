package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"go-bot/harness/agent"
	"go-bot/harness/schedule"
)

type NativeSchedule struct {
	Service     *schedule.Service
	SessionID   string
	Name        string
	Delegated   bool
	CWD         string
	AgentPreset string
}

const scheduleRRule = `{"type":"string","description":"RFC 5545 with exactly two lines: DTSTART (UTC Z or explicit IANA TZID), followed by RRULE. Use COUNT=1 for a one-time reminder. Supports MINUTELY, HOURLY, DAILY, WEEKLY, MONTHLY and YEARLY."}`
const scheduleModelTarget = `{"type":"string","enum":["this_session","new_once","new"],"description":"this_session delivers here; new_once creates a Session at the first delivery and reuses it; new creates one per delivery."}`

func (t NativeSchedule) Spec() agent.ToolSpec {
	spec := agent.ToolSpec{Name: t.Name}
	switch t.Name {
	case "schedule_create":
		spec.Description = "Create a scheduled task. Its instruction is delivered to the selected Session as a scheduled user message. Only the latest missed occurrence is delivered after downtime. Create or modify tasks only at the user's explicit request. Use the user's time zone when it is known."
		spec.Parameters = json.RawMessage(`{"type":"object","properties":{"title":{"type":"string","description":"Short task name of up to 120 characters"},"prompt":{"type":"string","description":"Instruction sent at each occurrence"},"rrule":` + scheduleRRule + `,"target":` + scheduleModelTarget + `},"required":["title","prompt","rrule","target"]}`)
	case "schedule_list":
		spec.Description = "List scheduled tasks across this Host, active first."
		spec.Parameters = json.RawMessage(`{"type":"object","properties":{"active":{"type":"boolean","description":"True for active only; false for paused and ended; omit for all"},"limit":{"type":"integer","minimum":1,"maximum":500},"offset":{"type":"integer","minimum":0}}}`)
		spec.ParallelSafe = true
	case "schedule_view":
		spec.Description = "Read one scheduled task on this Host by ID without activating its Session."
		spec.Parameters = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
		spec.ParallelSafe = true
	case "schedule_edit":
		spec.Description = "Update or delete a scheduled task by ID. Update preserves delivery history. Change tasks only at the user's explicit request. Use active false to pause or true to resume, and expectedUpdatedAt from your last read to detect conflicts."
		spec.Parameters = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"action":{"type":"string","enum":["update","delete"]},"title":{"type":"string"},"prompt":{"type":"string"},"rrule":` + scheduleRRule + `,"target":` + scheduleModelTarget + `,"active":{"type":"boolean"},"expectedUpdatedAt":{"type":"string"}},"required":["id","action"]}`)
	}
	return spec
}

type scheduleToolInput struct {
	ID                string          `json:"id"`
	Action            string          `json:"action"`
	Title             *string         `json:"title"`
	Prompt            *string         `json:"prompt"`
	RRule             *string         `json:"rrule"`
	Target            *string         `json:"target"`
	Active            *bool           `json:"active"`
	ExpectedUpdatedAt json.RawMessage `json:"expectedUpdatedAt"`
	Limit             *int            `json:"limit"`
	Offset            *int            `json:"offset"`
}

func (t NativeSchedule) decode(raw json.RawMessage) (scheduleToolInput, error) {
	var input scheduleToolInput
	fields := map[string][]string{
		"schedule_create": {"title", "prompt", "rrule", "target"},
		"schedule_list":   {"active", "limit", "offset"},
		"schedule_view":   {"id"},
		"schedule_edit":   {"id", "action", "title", "prompt", "rrule", "target", "active", "expectedUpdatedAt"},
	}
	projected, values, err := projectToolRoot(raw, fields[t.Name]...)
	if err != nil {
		return input, err
	}
	for _, value := range values {
		if strings.TrimSpace(string(value)) == "null" {
			return input, schedule.ErrInvalid
		}
	}
	if err := json.Unmarshal(projected, &input); err != nil {
		return input, err
	}
	return input, nil
}

// Validation of rule values belongs to the shared Schedule service. Validate
// only the request shape so delegated calls can receive the upstream refusal.
func (t NativeSchedule) Validate(raw json.RawMessage) error {
	input, err := t.decode(raw)
	if err != nil {
		return err
	}
	switch t.Name {
	case "schedule_create":
		if input.Title == nil || input.Prompt == nil || input.RRule == nil || input.Target == nil {
			return schedule.ErrInvalid
		}
	case "schedule_view", "schedule_edit":
		if input.ID == "" || strings.TrimSpace(input.ID) != input.ID {
			return schedule.ErrInvalid
		}
	}
	return nil
}

func (t NativeSchedule) target(choice *string) (*schedule.Target, error) {
	if choice == nil {
		return nil, nil
	}
	switch *choice {
	case "this_session":
		return &schedule.Target{Kind: "session", SessionID: t.SessionID}, nil
	case "new_once", "new":
		return &schedule.Target{Kind: *choice, Create: &schedule.TargetCreate{CWD: t.CWD, AgentPreset: t.AgentPreset}}, nil
	default:
		return nil, schedule.ErrInvalid
	}
}

func (t NativeSchedule) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if t.Delegated {
		return scheduleToolJSON(map[string]string{"code": "subagent_session", "message": "A delegated subagent cannot use scheduled tasks."})
	}
	if t.Service == nil {
		return "", schedule.ErrUnavailable
	}
	if err := t.Validate(raw); err != nil {
		return "", err
	}
	input, _ := t.decode(raw)
	target, err := t.target(input.Target)
	if err != nil {
		return "", err
	}
	var timing *schedule.Timing
	if input.RRule != nil {
		timing = &schedule.Timing{Kind: "rrule", RRule: input.RRule}
	}
	switch t.Name {
	case "schedule_create":
		if strings.TrimSpace(*input.Prompt) == "" {
			return scheduleToolJSON(map[string]string{"code": "invalid_prompt", "message": "Task instructions must not be empty."})
		}
		entry, err := t.Service.CreateTask(ctx, t.SessionID, schedule.CreateRequest{Title: *input.Title, Prompt: *input.Prompt, Timing: *timing, Target: target})
		return scheduleToolResult(input.ID, entry, err)
	case "schedule_list":
		limit, offset := 100, 0
		if input.Limit != nil {
			limit = *input.Limit
		}
		if input.Offset != nil {
			offset = *input.Offset
		}
		page, err := t.Service.ListTasks(input.Active, limit, offset)
		if err != nil {
			return scheduleToolResult(input.ID, schedule.Entry{}, err)
		}
		items := make([]map[string]any, 0, len(page.Items))
		for _, entry := range page.Items {
			items = append(items, modelSchedule(entry))
		}
		return scheduleToolJSON(map[string]any{"items": items, "shown": page.Shown, "total": page.Total, "limit": page.Limit, "offset": page.Offset})
	case "schedule_view":
		entry, err := t.Service.ViewTask(input.ID)
		return scheduleToolResult(input.ID, entry, err)
	case "schedule_edit":
		if input.Prompt != nil && strings.TrimSpace(*input.Prompt) == "" && input.Action == "update" {
			return scheduleToolJSON(map[string]string{"code": "invalid_prompt", "message": "Task instructions must not be empty."})
		}
		request := schedule.EditRequest{ID: input.ID, Action: input.Action, ExpectedUpdatedAt: input.ExpectedUpdatedAt}
		patch := schedule.Patch{Title: input.Title, Prompt: input.Prompt, Timing: timing, Target: target, Active: input.Active}
		if input.Action == "update" || patch != (schedule.Patch{}) {
			request.Patch = &patch
		}
		if (request.Action != "delete" && request.Action != "update") ||
			(request.Action == "delete" && request.Patch != nil) || (request.Action == "update" && patch == (schedule.Patch{})) {
			return scheduleToolJSON(map[string]string{"code": "invalid_selector", "message": "Update requires a patch. Delete accepts only the ID, action, and revision."})
		}
		entry, err := t.Service.EditTask(ctx, t.SessionID, request)
		if err == nil && input.Action == "delete" {
			return scheduleToolJSON(map[string]any{"id": input.ID, "deleted": true})
		}
		return scheduleToolResult(input.ID, entry, err)
	default:
		return "", schedule.ErrInvalid
	}
}

func modelSchedule(entry schedule.Entry) map[string]any {
	data, _ := json.Marshal(entry.Record)
	var result map[string]any
	_ = json.Unmarshal(data, &result)
	result["status"], result["target"], result["scheduledAtLocal"] = entry.Status, entry.Target, entry.ScheduledAtLocal
	if entry.UpdatedAt != "" {
		result["updatedAt"] = entry.UpdatedAt
	}
	if receipt := entry.LastDelivery; receipt != nil {
		result["lastDelivery"] = map[string]string{"scheduledAt": receipt.ScheduledAt, "deliveredAt": receipt.DeliveredAt, "targetSessionId": receipt.TargetSessionID}
	}
	return result
}

func scheduleToolResult(id string, entry schedule.Entry, err error) (string, error) {
	switch {
	case err == nil:
		return scheduleToolJSON(modelSchedule(entry))
	case errors.Is(err, schedule.ErrNotFound), errors.Is(err, schedule.ErrConflict):
		return scheduleToolJSON(map[string]string{"id": id, "code": err.Error()})
	case errors.Is(err, schedule.ErrInvalid):
		message := strings.TrimPrefix(err.Error(), schedule.ErrInvalid.Error()+"\n")
		code := "invalid_rule"
		switch {
		case strings.Contains(message, "future"):
			code = "not_future"
		case strings.Contains(message, "time_zone"):
			code = "invalid_time_zone"
		case strings.Contains(message, "minimum occurrence spacing"):
			code = "frequency_too_high"
		}
		return scheduleToolJSON(map[string]string{"code": code, "message": message})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", err
	default:
		return scheduleToolJSON(map[string]string{"code": "internal_error", "message": "The schedule operation failed."})
	}
}

func scheduleToolJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	return string(data), err
}
