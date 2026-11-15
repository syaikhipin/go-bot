package sessionformat

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ProcessClaims permits one active v4 writer per Session in a user Host.
// This prevents two local Session activations from opening the same journal
// at once. Host data-directory exclusivity is checked separately at startup;
// neither mechanism provides distributed multi-machine NAS coordination.
type ProcessClaims struct {
	mu    sync.Mutex
	owned map[string]bool
}

func (c *ProcessClaims) Acquire(ctx context.Context, sessionID string) (func(), error) {
	if c == nil || sessionID == "" {
		return nil, errors.New("v4 Session writer claim requires an owner and ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.owned == nil {
		c.owned = make(map[string]bool)
	}
	if c.owned[sessionID] {
		return nil, fmt.Errorf("Session %s already has a v4 writer", sessionID)
	}
	c.owned[sessionID] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.owned, sessionID)
			c.mu.Unlock()
		})
	}, nil
}
