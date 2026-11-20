package host

import (
	"context"
	"os"

	"go-bot/harness/sessionsearch"
	"go-bot/internal/hostruntime"
)

func historySnapshot(host *hostruntime.NativeHost) func(context.Context, string, sessionsearch.Cursor) (sessionsearch.Snapshot, error) {
	return func(ctx context.Context, id string, cursor sessionsearch.Cursor) (snapshot sessionsearch.Snapshot, err error) {
		err = host.WithSessionHistory(ctx, id, func() error {
			snapshot, err = readHistorySnapshot(ctx, host, id, cursor)
			return err
		})
		return snapshot, err
	}
}

func readHistorySnapshot(ctx context.Context, host *hostruntime.NativeHost, id string, cursor sessionsearch.Cursor) (sessionsearch.Snapshot, error) {
	archived, err := host.Archived(id)
	if err != nil {
		return sessionsearch.Snapshot{}, err
	}
	if archived {
		return sessionsearch.Snapshot{}, os.ErrNotExist
	}
	if events, reset, found, err := host.DurableSessionSuffix(id, cursor.Next); found {
		if err == nil {
			next := cursor.Next + len(events)
			if reset {
				next = len(events)
			}
			// A live snapshot is based on the fsync watermark rather than stat.
			return sessionsearch.Snapshot{Cursor: sessionsearch.Cursor{Next: next, Size: -1}, Events: events, Reset: reset}, nil
		}
		// Session shutdown may have detached its live journal during lookup.
	}
	info, err := host.SessionLogInfo(id)
	if err != nil {
		return sessionsearch.Snapshot{}, err
	}
	snapshot := sessionsearch.Snapshot{Cursor: sessionsearch.Cursor{Next: cursor.Next, Size: info.Size(), ModTime: info.ModTime().UnixNano()}}
	if snapshot.Cursor == cursor {
		return snapshot, nil
	}
	artifact, err := host.ReadSession(ctx, id)
	if err != nil {
		return sessionsearch.Snapshot{}, err
	}
	next := cursor.Next
	if cursor.Size < 0 || info.Size() <= cursor.Size || next > len(artifact.Events) {
		next = 0
		snapshot.Reset = true
	}
	snapshot.Next = len(artifact.Events)
	snapshot.Events = artifact.Events[next:]
	return snapshot, nil
}
