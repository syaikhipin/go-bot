package schedule

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionArchiveGuardSerializesWithScheduleTargets(t *testing.T) {
	ctx := t.Context()
	service, err := NewHostService(FileStore{Path: filepath.Join(t.TempDir(), "schedule.json")}, noOpHostPrepare,
		func(context.Context, string, string) (string, error) { return "", errors.New("unexpected delivery") })
	if err != nil {
		t.Fatal(err)
	}
	var archived bool
	errArchived := errors.New("destination archived")
	service.SetSessionTargetCheck(func(_ context.Context, id string) error {
		if id == "destination" && archived {
			return errArchived
		}
		return nil
	})
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	create := func(now time.Time) (Record, error) {
		return NewAfter("task", "Task", "Prompt", 3600, now)
	}
	task, err := service.CreateForTarget(ctx, "origin", &Target{Kind: "session", SessionID: "destination"}, create)
	if err != nil {
		t.Fatal(err)
	}
	commit := func() error { archived = true; return nil }
	if err := service.GuardSessionArchive("destination", commit); !errors.Is(err, ErrSessionHasSchedule) || archived {
		t.Fatalf("active destination accepted archive: %v", err)
	}
	if _, err := service.EditFor(ctx, "origin", task.Record.ID, task.Record, "pause", func(record Record, _ time.Time) (Record, error) {
		return record, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.GuardSessionArchive("destination", commit); err != nil || !archived {
		t.Fatalf("paused destination blocked archive: %v", err)
	}
	if _, err := service.DeleteFor(ctx, "origin", task.Record.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.GuardSessionArchive("destination", commit); err != nil || !archived {
		t.Fatalf("unbound destination could not be archived: %v", err)
	}
	if _, err := service.CreateForTarget(ctx, "origin", &Target{Kind: "session", SessionID: "destination"}, create); !errors.Is(err, errArchived) {
		t.Fatalf("created a task targeting an archived session: %v", err)
	}
}
