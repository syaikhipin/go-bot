package agentfs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	ReadLineLimit      int64 = 2000
	readMaxLineLength        = 2000 // JavaScript UTF-16 code units
	readMaxOutputBytes       = 50 * 1024
)

type TextLine struct {
	Number int64
	Text   string
}

type LineReadResult struct {
	ReadResult
	Offset           int64
	Lines            []TextLine
	TotalLines       int64
	TruncatedByBytes bool
}

// ReadLines keeps one open file handle throughout the scan. The selected
// window is bounded even when a file has a newline-free giant line; the scan
// continues to EOF so totalLines and an out-of-range offset are authoritative.
func (v ContainerView) ReadLines(ctx context.Context, path string, offset, limit int64) (LineReadResult, error) {
	if offset < 1 || limit < 1 || limit > ReadLineLimit {
		return LineReadResult{}, errors.New("read offset and limit must be positive lines within the read limit")
	}
	path, err := v.path(path)
	if err != nil {
		return LineReadResult{}, err
	}
	file, err := openContainerExisting(path, os.O_RDONLY, containerRegular)
	if err != nil {
		return LineReadResult{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return LineReadResult{}, err
	}
	result, err := scanLineWindow(ctx, file, offset, limit)
	if err != nil {
		return LineReadResult{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return LineReadResult{}, err
	}
	if fileVersion(before) != fileVersion(after) {
		return LineReadResult{}, ErrChanged
	}
	target, _, err := containerMutationTarget(path)
	if err != nil {
		return LineReadResult{}, err
	}
	targetInfo, err := os.Stat(target)
	if err != nil || !os.SameFile(before, targetInfo) || fileVersion(targetInfo) != fileVersion(before) {
		return LineReadResult{}, ErrChanged
	}
	result.ResolvedPath = resolvedOpenFilePath(file, path)
	result.observedTarget = target
	result.observedVersion = fileVersion(before)
	if !result.TruncatedByBytes && offset > result.TotalLines && !(result.TotalLines == 0 && offset == 1) {
		return LineReadResult{}, fmt.Errorf("offset %d is out of range for %q (%d lines)", offset, path, result.TotalLines)
	}
	return result, nil
}

func scanLineWindow(ctx context.Context, file *os.File, offset, limit int64) (LineReadResult, error) {
	result := LineReadResult{Offset: offset}
	reader := bufio.NewReaderSize(file, 64*1024)
	units := make([]uint16, 0, readMaxLineLength+1)
	outputBytes := 0
	lineHasContent := false
	flush := func() {
		result.TotalLines++
		line := units
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if !result.TruncatedByBytes && result.TotalLines >= offset && int64(len(result.Lines)) < limit {
			text := string(utf16.Decode(line))
			if len(line) > readMaxLineLength {
				text = string(utf16.Decode(line[:readMaxLineLength])) + "... (line truncated to 2000 chars)"
			}
			bytes := len(text)
			if len(result.Lines) > 0 {
				bytes++
			}
			if outputBytes+bytes > readMaxOutputBytes {
				result.TruncatedByBytes = true
			} else {
				outputBytes += bytes
				result.Lines = append(result.Lines, TextLine{Number: result.TotalLines, Text: text})
			}
		}
		units = units[:0]
		lineHasContent = false
	}
	byteOffset := int64(0)
	for scanned := 0; ; scanned++ {
		if scanned%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return LineReadResult{}, err
			}
		}
		r, size, err := reader.ReadRune()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return LineReadResult{}, err
		}
		if (r == utf8.RuneError && size == 1) || (r == 0 && byteOffset < 8192) {
			return LineReadResult{}, errors.New("readfile requires UTF-8 text without NUL bytes; use an appropriate binary or image tool")
		}
		if byteOffset == 0 && r == '\ufeff' {
			byteOffset += int64(size)
			continue
		}
		byteOffset += int64(size)
		if r == '\n' {
			flush()
			continue
		}
		lineHasContent = true
		if len(units) >= readMaxLineLength+1 {
			continue
		}
		if r <= 0xffff {
			units = append(units, uint16(r))
		} else {
			value := uint32(r) - 0x10000
			units = append(units, uint16(0xd800+(value>>10)), uint16(0xdc00+(value&0x3ff)))
			if len(units) > readMaxLineLength+1 {
				units = units[:readMaxLineLength+1]
			}
		}
	}
	if lineHasContent {
		flush()
	}
	return result, nil
}
