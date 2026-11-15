package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type SessionListPage struct {
	Headers    []Header `json:"sessions"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

// ListSessions reads file-backed Session headers without loading all events.
func (s FileStore) ListSessions(ctx context.Context, userID, cursor string, limit int) (SessionListPage, error) {
	if !safePathComponent.MatchString(userID) || limit < 1 || limit > 100 || (cursor != "" && !safePathComponent.MatchString(cursor)) {
		return SessionListPage{}, errors.New("invalid Session list request")
	}
	if !filepath.IsAbs(s.Root) {
		return SessionListPage{}, errors.New("absolute Session root is required")
	}
	base := filepath.Join(s.Root, "users", userID, "sessions")
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return SessionListPage{Headers: []Header{}}, nil
	}
	if err != nil {
		return SessionListPage{}, err
	}
	page := SessionListPage{Headers: make([]Header, 0, limit)}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return SessionListPage{}, err
		}
		id := entry.Name()
		if id <= cursor || !safePathComponent.MatchString(id) || !entry.IsDir() {
			continue
		}
		header, err := s.ReadHeader(userID, id)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotSessionHeader) {
			continue
		}
		if err != nil {
			return SessionListPage{}, fmt.Errorf("read Session %s header: %w", id, err)
		}
		if len(page.Headers) == limit {
			page.NextCursor = page.Headers[len(page.Headers)-1].SessionID
			break
		}
		page.Headers = append(page.Headers, header)
	}
	return page, nil
}
