package executor

import (
	"context"
	"errors"

	"go-harness/harness/assembly"
	"go-harness/harness/session"
)

// NativeFileStorage binds one Agent Session to the standalone cloud
// computer's local v4 JSONL backend. Child and grandchild Sessions remain
// sibling directories under the same initial project cwd.
type NativeFileStorage struct {
	Store     sessionformat.FileStore
	SessionID string
}

var _ assembly.NativeStorage = NativeFileStorage{}
var _ assembly.NativeChildStorageFactory = NativeFileStorage{}

func (s NativeFileStorage) CreateNative(ctx context.Context, header sessionformat.Header) (sessionformat.EventJournal, error) {
	if header.ID != s.SessionID {
		return nil, errors.New("local Session storage ID mismatch")
	}
	return s.Store.Create(ctx, header)
}

func (s NativeFileStorage) OpenNative(ctx context.Context) (sessionformat.EventJournal, sessionformat.PhysicalArtifact, error) {
	return s.Store.Open(ctx, s.SessionID)
}

func (s NativeFileStorage) ChildNativeStorage(childID string) (assembly.NativeStorage, error) {
	if childID == "" || childID == s.SessionID || childID == s.Store.RootSessionID {
		return nil, errors.New("invalid local child Session ID")
	}
	child := s
	if child.Store.RootSessionID == "" {
		child.Store.RootSessionID = s.SessionID
	}
	child.SessionID = childID
	if _, err := child.Store.Path(childID); err != nil {
		return nil, err
	}
	return child, nil
}
