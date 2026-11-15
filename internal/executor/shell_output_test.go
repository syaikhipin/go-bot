package executor

import (
	"os"
	"strings"
	"testing"
)

func TestShellOutputKeepsTailAndSpillsCompleteStream(t *testing.T) {
	w := &shellOutputCollector{limit: 4, tempDir: t.TempDir()}
	if n, err := w.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("first write = %d, %v", n, err)
	}
	if n, err := w.Write([]byte("def")); err != nil || n != 3 {
		t.Fatalf("second write = %d, %v", n, err)
	}
	path := w.finish()
	if !w.truncated || string(w.tail) != "cdef" || path == "" {
		t.Fatalf("tail=%q truncated=%t spill=%q", w.tail, w.truncated, path)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "abcdef" {
		t.Fatalf("spill=%q, %v", data, err)
	}
}

func TestShellOutputExactLimitNeedsNoSpill(t *testing.T) {
	w := &shellOutputCollector{limit: 4, tempDir: t.TempDir()}
	if _, err := w.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if w.truncated || string(w.tail) != "abcd" || w.finish() != "" {
		t.Fatalf("exact-limit output=%q truncated=%t", w.tail, w.truncated)
	}
}

func TestShellOutputOverSpillCapRetainsTailButRemovesIncompleteFile(t *testing.T) {
	dir := t.TempDir()
	w := &shellOutputCollector{limit: 4, spillLimit: 7, tempDir: dir}
	for _, chunk := range []string{"abc", "def", "ghij"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if !w.truncated || string(w.tail) != "ghij" || w.finish() != "" {
		t.Fatalf("over-cap output=%q truncated=%t", w.tail, w.truncated)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("incomplete spill retained: %v, %v", entries, err)
	}
	// A single large pipe read is also bounded without allocating that read
	// into an intermediate in-memory prefix.
	w = &shellOutputCollector{limit: 4, spillLimit: 7, tempDir: dir}
	if _, err := w.Write([]byte(strings.Repeat("x", 100))); err != nil || string(w.tail) != "xxxx" || w.finish() != "" {
		t.Fatalf("large chunk tail=%q, %v", w.tail, err)
	}
}
