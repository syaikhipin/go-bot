package sessionformat

import (
	"context"
	"errors"
	"testing"
)

func TestProcessClaimsOwnsEachV4SessionOnce(t *testing.T) {
	claims := &ProcessClaims{}
	first, err := claims.Acquire(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claims.Acquire(context.Background(), "first"); err == nil {
		t.Fatal("second writer acquired the same Session")
	}
	second, err := claims.Acquire(context.Background(), "second")
	if err != nil {
		t.Fatalf("independent Session writer was rejected: %v", err)
	}
	first()
	first()
	if again, err := claims.Acquire(context.Background(), "first"); err != nil {
		t.Fatalf("released Session could not be reopened: %v", err)
	} else {
		again()
	}
	second()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := claims.Acquire(ctx, "first"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled writer claim returned %v", err)
	}
}
