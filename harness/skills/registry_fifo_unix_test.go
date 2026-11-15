//go:build linux || darwin

package skills

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRegistrySkipsFIFOAndRechecksLocator(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	path := filepath.Join(home, "skills/demo/SKILL.md")
	write(t, path, document("demo", "", "Body"))
	registry := Registry{DSHHome: home}
	entry, found, err := registry.Find(ctx, Lookup{}, "demo")
	if err != nil || !found {
		t.Fatalf("discovery: %t, %v", found, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := entry.Load(ctx); err != nil || found {
		t.Fatalf("FIFO load: %t, %v", found, err)
	}
	if snapshot, err := registry.Snapshot(ctx, Lookup{}); err != nil || !snapshot.Complete || len(snapshot.Entries) != 0 {
		t.Fatalf("FIFO discovery: %+v, %v", snapshot, err)
	}
}
