package executor

import (
	"os"
)

const maxShellSpillBytes int64 = 16 << 20

// shellOutputCollector keeps the most recent output for the model. Once the
// stream exceeds its in-memory cap, it also tries to preserve the complete
// stream in one private temporary file. A failed/full spill never stops the
// command: the tail remains available and the result marks the full output
// unavailable. Each collector has one writer goroutine and is finalized only
// after the command's stdout/stderr copy has stopped.
type shellOutputCollector struct {
	tail        []byte
	limit       int
	truncated   bool
	tempDir     string
	spillLimit  int64
	spill       *os.File
	spillBytes  int64
	spillFailed bool
}

func (w *shellOutputCollector) Write(p []byte) (int, error) {
	original := len(p)
	if original == 0 {
		return 0, nil
	}
	if !w.truncated && original > w.limit-len(w.tail) {
		w.truncated = true
		w.startSpill()
	}
	if w.spill != nil {
		limit := w.spillLimit
		if limit <= 0 {
			limit = maxShellSpillBytes
		}
		if int64(original) > limit-w.spillBytes {
			w.discardSpill()
		} else if n, err := w.spill.Write(p); err != nil || n != original {
			w.discardSpill()
		} else {
			w.spillBytes += int64(original)
		}
	}
	if original >= w.limit {
		w.tail = append(w.tail[:0], p[original-w.limit:]...)
		return original, nil
	}
	if excess := len(w.tail) + original - w.limit; excess > 0 {
		copy(w.tail, w.tail[excess:])
		w.tail = w.tail[:len(w.tail)-excess]
	}
	w.tail = append(w.tail, p...)
	return original, nil
}

func (w *shellOutputCollector) startSpill() {
	if w.spillFailed || w.limit <= 0 {
		return
	}
	dir := w.tempDir
	if dir == "" {
		dir = os.TempDir()
	}
	file, err := os.CreateTemp(dir, "serveragent-shell-output-*")
	if err != nil {
		w.spillFailed = true
		return
	}
	w.spill = file
	if len(w.tail) > 0 {
		if n, err := file.Write(w.tail); err != nil || n != len(w.tail) {
			w.discardSpill()
			return
		}
		w.spillBytes = int64(len(w.tail))
	}
}

func (w *shellOutputCollector) discardSpill() {
	w.spillFailed = true
	if w.spill == nil {
		return
	}
	path := w.spill.Name()
	_ = w.spill.Close()
	_ = os.Remove(path)
	w.spill = nil
}

// finish returns a path only if the complete stream was written and closed.
// The caller may expose the path to this container's Agent file tools.
func (w *shellOutputCollector) finish() string {
	if w.spill == nil {
		return ""
	}
	path := w.spill.Name()
	if err := w.spill.Close(); err != nil {
		_ = os.Remove(path)
		w.spill = nil
		return ""
	}
	w.spill = nil
	return path
}
