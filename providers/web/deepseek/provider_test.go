package deepseek

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSearchSlotsUseSlidingWindow(t *testing.T) {
	var provider Provider
	for range 5 {
		if err := provider.waitForSlot(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := provider.waitForSlot(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sixth request was not held for the next window: %v", err)
	}
	for i := range provider.starts {
		provider.starts[i] = provider.starts[i].Add(-2 * time.Second)
	}
	if err := provider.waitForSlot(t.Context()); err != nil || len(provider.starts) != 1 {
		t.Fatalf("expired slots were not released: %v", err)
	}
}
