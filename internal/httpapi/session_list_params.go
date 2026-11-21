package httpapi

import (
	"errors"
	"go-bot/harness/session"
	"strconv"
	"time"

	"github.com/google/uuid"
)

func sessionListPageParams(limitRaw, cursor string) (int, error) {
	limit := 30
	if limitRaw != "" {
		parsed, err := strconv.Atoi(limitRaw)
		if err != nil || parsed < 1 || parsed > 100 {
			return 0, errors.New("invalid Session page size")
		}
		limit = parsed
	}
	if cursor != "" && uuid.Validate(cursor) != nil {
		return 0, errors.New("invalid Session page cursor")
	}
	return limit, nil
}

// UserSessionPage is a public projection; it carries no infrastructure address.
type UserSessionPage struct {
	Sessions   []ListedUserSession `json:"sessions"`
	NextCursor string              `json:"nextCursor,omitempty"`
}
type ListedUserSession struct {
	IsMain    bool      `json:"isMain,omitempty"`
	SessionID string    `json:"sessionId"`
	CreatedAt time.Time `json:"createdAt"`
	Mode      string    `json:"mode"`
	Title     string    `json:"title,omitempty"`
	CWD       string    `json:"cwd,omitempty"`
}

func listedUserPage(page sessionformat.SessionListPage, titleFor func(string) string) (UserSessionPage, error) {
	result := UserSessionPage{Sessions: make([]ListedUserSession, 0, len(page.Sessions)), NextCursor: page.NextCursor}
	for _, h := range page.Sessions {
		row := ListedUserSession{SessionID: h.ID, CreatedAt: time.UnixMilli(h.CreatedAt).UTC(), Mode: "host", CWD: h.CWD}
		if titleFor != nil {
			row.Title = titleFor(h.ID)
		}
		result.Sessions = append(result.Sessions, row)
	}
	return result, nil
}
