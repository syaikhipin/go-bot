package memory

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMemoryBatchAndConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	var group sync.WaitGroup
	for _, text := range []string{"日报用中文", "周报用英文"} {
		group.Go(func() {
			if _, err := store.Apply("user", []Operation{{Action: "add", Content: text}}); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	path := filepath.Join(dir, "USER.md")
	original, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(original), "日报用中文") || !strings.Contains(string(original), "周报用英文") {
		t.Fatalf("concurrent memory = %q, %v", original, err)
	}
	if result, err := store.Apply("user", []Operation{{Action: "add", Content: "日报用中文"}}); err != nil || result.Entries != 2 {
		t.Fatalf("duplicate result = %+v, %v", result, err)
	}
	for _, ops := range [][]Operation{
		{{Action: "patch", OldText: "报", Content: "报告"}},
		{{Action: "remove", OldText: "周报"}, {Action: "add", Content: strings.Repeat("字", 1376)}},
	} {
		if _, err := store.Apply("user", ops); err == nil {
			t.Fatalf("invalid batch accepted: %v", ops[0].Action)
		}
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(original) {
		t.Fatalf("failed batch changed memory: %q, %v", after, err)
	}
	result, err := store.Apply("user", []Operation{
		{Action: "patch", OldText: "周报用英文", Content: "周报"},
		{Action: "add", Content: strings.Repeat("字", 1362)},
	})
	if err != nil || result.Characters != 1375 {
		t.Fatalf("replacement batch = %+v, %v", result, err)
	}
	invalid := []byte{0xff}
	if err := os.WriteFile(path, invalid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply("user", []Operation{{Action: "add", Content: "new"}}); err == nil {
		t.Fatal("invalid file was overwritten")
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(invalid) {
		t.Fatalf("invalid file was changed: %q, %v", after, err)
	}
}
