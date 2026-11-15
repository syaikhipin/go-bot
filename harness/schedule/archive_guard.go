package schedule

import (
	"context"
	"errors"
	"log"
)

var ErrSessionHasSchedule = errors.New("session is the target of a scheduled reminder")

func (s *Service) GuardSessionArchive(id string, archive func() error) error {
	return s.ArchiveSession(context.Background(), id, false, archive)
}

// ArchiveSession serializes the archive marker and optional removal of
// armed tasks with task edits and delivery. The marker remains authoritative
// if a post-archive cleanup fails.
func (s *Service) ArchiveSession(ctx context.Context, id string, stopActivity bool, archive func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed || s.poisoned {
		return ErrUnavailable
	}
	var armed []Task
	for _, taskID := range s.order {
		task := s.tasks[taskID]
		if task.Status == "active" && task.deliverySession() == id {
			armed = append(armed, task)
		}
	}
	if len(armed) != 0 && !stopActivity {
		return ErrSessionHasSchedule
	}
	if err := archive(); err != nil {
		return err
	}
	if len(armed) == 0 {
		return nil
	}
	next := make(map[string]Task, len(s.tasks)-len(armed))
	removed := make(map[string]bool, len(armed))
	for _, task := range armed {
		removed[task.Record.ID] = true
	}
	order := make([]string, 0, len(s.tasks)-len(armed))
	for _, taskID := range s.order {
		if !removed[taskID] {
			next[taskID] = s.tasks[taskID]
			order = append(order, taskID)
		}
	}
	if err := s.saveLocked(context.WithoutCancel(ctx), next, order, true); err != nil {
		log.Printf("Schedule: remove active tasks for archived Session %s: %v", id, err)
		return nil
	}
	for _, task := range armed {
		delete(s.executing, task.Record.ID)
		s.queueNoticeLocked(task, id, "deleted")
	}
	return nil
}

func (s *Service) SetSessionTargetCheck(check HostPrepare) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkSessionTarget = check
}
