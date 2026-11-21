// Package proactive owns Host-side wake scheduling and the background Agent's
// result contract. It does not implement an Agent loop or a second task queue.
package proactive

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

type Check struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
}

type Evidence struct {
	Title     string `json:"title"`
	Reference string `json:"reference"`
}

type Notification struct {
	Message  string     `json:"message"`
	Evidence []Evidence `json:"evidence"`
}

type Result struct {
	WorkSummary string        `json:"workSummary"`
	NextCheck   Check         `json:"nextCheck"`
	Notify      *Notification `json:"notify"`
}

// DecodeResult is also used inside the finishing tool, before it requests
// ConcludeTurn. A bad time must be correctable in the current ordinary turn.
// Replays decode without a future-time check: a once-valid decision can now
// be overdue and should produce one wake, not disappear from recovery.
func DecodeResult(raw json.RawMessage, now time.Time) (Result, error) {
	var result Result
	if len(raw) > 48<<10 {
		return result, errors.New("proactive result exceeds 48 KiB")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return result, errors.New("proactive result must be an object")
	}
	for _, name := range []string{"workSummary", "nextCheck", "notify"} {
		if _, ok := fields[name]; !ok {
			return result, fmt.Errorf("proactive result requires %s", name)
		}
	}
	if bytes.Equal(bytes.TrimSpace(fields["nextCheck"]), []byte("null")) {
		return result, errors.New("nextCheck must contain a future at and a reason; null is not allowed")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if !errors.Is(decoder.Decode(new(any)), io.EOF) {
		return result, errors.New("proactive result must contain exactly one object")
	}
	if strings.TrimSpace(result.WorkSummary) == "" || len([]rune(result.WorkSummary)) > 4000 {
		return result, errors.New("workSummary must contain 1–4000 characters")
	}
	if strings.TrimSpace(result.NextCheck.Reason) == "" || len([]rune(result.NextCheck.Reason)) > 1000 {
		return result, errors.New("nextCheck.reason must contain 1–1000 characters")
	}
	at, err := time.Parse(time.RFC3339Nano, result.NextCheck.At)
	if err != nil {
		return result, errors.New("nextCheck.at must be an absolute RFC 3339 time with a timezone")
	}
	if !now.IsZero() && at.UnixMilli() <= now.UnixMilli() {
		return result, errors.New("nextCheck.at must be in the future; sample the current time and choose again")
	}
	if result.Notify != nil {
		if strings.TrimSpace(result.Notify.Message) == "" || len([]rune(result.Notify.Message)) > 8000 || result.Notify.Evidence == nil || len(result.Notify.Evidence) > 20 {
			return result, errors.New("notify requires a message of 1–8000 characters and an evidence array of at most 20 items")
		}
		for _, item := range result.Notify.Evidence {
			if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.Reference) == "" || len(item.Title) > 500 || len(item.Reference) > 2000 {
				return result, errors.New("evidence requires a title and reference")
			}
		}
	}
	return result, nil
}

func (c Check) UnixMilli() int64 {
	at, _ := time.Parse(time.RFC3339Nano, c.At)
	return at.UnixMilli()
}

func ResultSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"workSummary", "nextCheck", "notify"},
		"properties": map[string]any{
			"workSummary": map[string]any{"type": "string", "minLength": 1, "maxLength": 4000},
			"nextCheck": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"at", "reason"},
				"properties": map[string]any{
					"at":     map[string]any{"type": "string", "description": "An absolute future RFC 3339 time including timezone. Always arrange a next check, even if nothing merits contacting the user now."},
					"reason": map[string]any{"type": "string", "minLength": 1, "maxLength": 1000},
				},
			},
			"notify": map[string]any{"anyOf": []any{
				map[string]any{"type": "null"},
				map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"message", "evidence"},
					"properties": map[string]any{
						"message": map[string]any{"type": "string", "minLength": 1, "maxLength": 8000, "description": "Self-contained verified findings for the main Agent to convey directly to the user; not a request to decide again whether to notify."},
						"evidence": map[string]any{"type": "array", "maxItems": 20, "items": map[string]any{
							"type": "object", "additionalProperties": false, "required": []string{"title", "reference"},
							"properties": map[string]any{"title": map[string]any{"type": "string", "minLength": 1, "maxLength": 500}, "reference": map[string]any{"type": "string", "minLength": 1, "maxLength": 2000}},
						}},
					},
				},
			}},
		},
	}
}
