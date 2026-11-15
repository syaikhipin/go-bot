package session

import (
	"context"
	"time"

	"github.com/google/uuid"
)

func noOpClaim(context.Context, string) (func(), error) {
	return func() {}, nil
}

func appendStoreHeader() Header {
	id := uuid.NewString()
	return Header{
		SchemaVersion: SchemaVersion,
		SessionID:     id,
		UserID:        "alice",
		Mode:          "container",
		Workspace:     "/workspace",
		CreatedAt:     time.Now().UTC(),
	}
}
