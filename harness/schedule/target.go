package schedule

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Target describes where a durable reminder is delivered. New destinations
// are reserved durably before the Host attempts to create a Session.
type Target struct {
	Kind      string        `json:"kind"`
	SessionID string        `json:"sessionId,omitempty"`
	Create    *TargetCreate `json:"create,omitempty"`
}

type TargetCreate struct {
	CWD         string `json:"cwd,omitempty"`
	AgentPreset string `json:"agentPreset,omitempty"`
}

type HostCreateDestination func(context.Context, string, TargetCreate, string) error

func (s *Service) SetDestinationCreator(create HostCreateDestination) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createDestination = create
}

type PendingDelivery struct {
	TargetSessionID string `json:"targetSessionId"`
}

func fillReceiptTarget(task *Task, sessionID string) {
	if task.LastDelivery != nil && task.LastDelivery.TargetSessionID == "" {
		task.LastDelivery.TargetSessionID = sessionID
	}
	for i := range task.DeliveryHistory.Records {
		if task.DeliveryHistory.Records[i].TargetSessionID == "" {
			task.DeliveryHistory.Records[i].TargetSessionID = sessionID
		}
	}
}

func (task Task) visibleTo(sessionID string) bool {
	return task.SessionID == sessionID || slices.Contains(task.InvolvedSessionIDs, sessionID) ||
		(task.Target != nil && task.Target.Kind == "session" && task.Target.SessionID == sessionID)
}

func (task Task) deliverySession() string {
	if task.Target == nil {
		return task.SessionID
	}
	return task.Target.SessionID
}

func validateTarget(target Target) error {
	switch target.Kind {
	case "session":
		if target.SessionID != "" && strings.TrimSpace(target.SessionID) == target.SessionID && target.Create == nil {
			return nil
		}
	case "new_once", "new":
		if (target.Kind == "new" && target.SessionID == "") ||
			(target.Kind == "new_once" && (target.SessionID == "" || uuid.Validate(target.SessionID) == nil)) {
			if target.Create == nil || ((target.Create.CWD == "" || (filepath.IsAbs(target.Create.CWD) && len(target.Create.CWD) <= 4096)) && len(target.Create.AgentPreset) <= 256) {
				return nil
			}
		}
	}
	return errors.New("invalid or unsupported Schedule target")
}

func (s *Service) configureTarget(ctx context.Context, target Target, prior *Target) (*Target, error) {
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	if target.Kind != "session" && s.createSession == nil && s.createDestination == nil {
		return nil, errors.New("Schedule Session creation is unavailable")
	}
	if target.Kind == "new_once" && target.SessionID == "" {
		if prior != nil && prior.Kind == "new_once" {
			target.SessionID = prior.SessionID
		}
		if target.SessionID == "" {
			target.SessionID = uuid.NewString()
		}
	}
	if s.checkSessionTarget != nil && target.Kind == "session" {
		if err := s.checkSessionTarget(ctx, target.SessionID); err != nil {
			return nil, err
		}
	}
	if target.Create != nil {
		create := *target.Create
		target.Create = &create
	}
	return &target, nil
}

func (s *Service) prepareTask(ctx context.Context, task Task) (Task, string, error) {
	if task.Target == nil || task.Target.Kind == "session" {
		id := task.deliverySession()
		return task, id, s.prepareSession(ctx, id)
	}
	if s.createSession == nil && s.createDestination == nil {
		return task, "", errors.New("Schedule Session creation is unavailable")
	}
	id := task.Target.SessionID
	if task.Target.Kind == "new" {
		if task.PendingDelivery == nil {
			task.PendingDelivery = &PendingDelivery{TargetSessionID: uuid.NewString()}
			if err := s.mutateLocked(ctx, task.Record.ID, &task, false); err != nil {
				return task, "", err
			}
		}
		id = task.PendingDelivery.TargetSessionID
	}
	if s.createDestination != nil {
		create := TargetCreate{}
		if task.Target.Create != nil {
			create = *task.Target.Create
		}
		title := task.Record.Title
		if task.Target.Kind == "new" {
			record := task.Record
			record.ScheduledAt = s.now().UTC().Format(time.RFC3339Nano)
			if local := strings.Replace(localScheduledAt(record), "T", " ", 1); len(local) >= 16 {
				title += " · " + local[:16]
			}
		}
		return task, id, s.createDestination(ctx, id, create, title)
	}
	return task, id, s.createSession(ctx, id)
}

func taskStatusValid(status string) bool {
	return status == "active" || status == "paused" || status == "inactive" || status == "ended"
}
