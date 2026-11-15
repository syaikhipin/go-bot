package agentfs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrAmbiguousEdit = errors.New("old text must occur exactly once")
var ErrChanged = errors.New("file changed since it was read")

type ReadResult struct {
	Content []byte
	// ResolvedPath is the absolute file path represented by the opened handle.
	// It is presentation metadata, not a new authorization decision.
	ResolvedPath string
	// SHA256 identifies the complete opened file, never just Content's
	// displayed prefix. It is empty if a bounded read cannot reach EOF.
	SHA256    string
	Truncated bool
	// A native observed view carries these internally until the file tool has
	// validated the model-facing text and confirms a successful read.
	observedTarget  string
	observedVersion string
}

func resolvedOpenFilePath(file *os.File, fallback string) string {
	// Linux's container opener reopens a validated O_PATH handle through
	// /proc/self/fd. os.File.Name() retains the *old* O_PATH descriptor's
	// spelling after reopening; use the live descriptor number instead.
	if resolved, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", file.Fd())); err == nil {
		if filepath.IsAbs(resolved) && !strings.HasSuffix(resolved, " (deleted)") {
			return filepath.Clean(resolved)
		}
		return fallback
	}
	// Developer hosts do not use the Linux handle opener. Their resolved
	// spelling is best effort; the production container uses the handle above.
	if resolved, err := filepath.EvalSymlinks(fallback); err == nil {
		return resolved
	}
	return fallback
}

const maxPartialReadHashBytes int64 = 1 << 20

// A partial model-facing read can still carry a usable edit version when the
// whole file fits the edit tool's 1 MiB limit. Larger files remain bounded and
// deliberately have no digest instead of a misleading prefix digest.
func readBoundedFile(file *os.File, maxBytes int64) (ReadResult, error) {
	readLimit := max(maxBytes, maxPartialReadHashBytes)
	data, err := io.ReadAll(io.LimitReader(file, readLimit+1))
	if err != nil {
		return ReadResult{}, err
	}
	complete := int64(len(data)) <= readLimit
	result := ReadResult{Truncated: int64(len(data)) > maxBytes}
	if complete {
		result.SHA256 = digest(data)
	}
	if result.Truncated {
		data = data[:maxBytes]
	}
	result.Content = data
	return result, nil
}

func readBoundedFileAt(file *os.File, offset, maxBytes int64) (ReadResult, error) {
	if offset < 0 || maxBytes <= 0 || maxBytes > 64<<20 {
		return ReadResult{}, errors.New("read offset must be nonnegative and limit must be between 1 and 67108864 bytes")
	}
	if offset == 0 && maxBytes >= maxPartialReadHashBytes {
		return readBoundedFile(file, maxBytes)
	}
	// A clearly large file needs no speculative 1 MiB prefix read on every
	// page. A stale size can only suppress an optional digest, never invent one.
	if info, err := file.Stat(); err == nil && info.Size() > maxPartialReadHashBytes {
		return readLargeFileWindow(file, offset, maxBytes)
	}
	// Read at most the editable prefix first. When it reaches EOF, even a
	// later window can carry the complete file's version without another read.
	head, err := io.ReadAll(io.LimitReader(file, maxPartialReadHashBytes+1))
	if err != nil {
		return ReadResult{}, err
	}
	if int64(len(head)) <= maxPartialReadHashBytes {
		start := min(offset, int64(len(head)))
		end := int64(len(head))
		if maxBytes < end-start {
			end = start + maxBytes
		}
		return ReadResult{Content: head[start:end], SHA256: digest(head), Truncated: end < int64(len(head))}, nil
	}
	return readLargeFileWindow(file, offset, maxBytes)
}

func readLargeFileWindow(file *os.File, offset, maxBytes int64) (ReadResult, error) {
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return ReadResult{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return ReadResult{}, err
	}
	result := ReadResult{Truncated: int64(len(data)) > maxBytes}
	if result.Truncated {
		data = data[:maxBytes]
	} else if offset == 0 {
		result.SHA256 = digest(data)
	}
	result.Content = data
	return result, nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// A short write is a failed mutation even if the filesystem returned no
// error. In particular, never return the digest of bytes not fully written.
func writeAll(file io.Writer, data []byte) error {
	n, err := file.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}
