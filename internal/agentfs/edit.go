package agentfs

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var ErrEditTooLarge = errors.New("edited file exceeds size limit")
var ErrEditNotText = errors.New("editfile requires UTF-8 text without NUL bytes")

// applyBoundedLiteralEdit matches exact text after normalizing CRLF to LF, as
// DeepSeek's readForEdit/applyLiteralEdit does. It restores the original line
// ending style after replacement and checks both intermediate and final sizes
// before allocating the potentially amplified result.
func applyBoundedLiteralEdit(current []byte, oldText, newText string, replaceAll bool, maxBytes int64) ([]byte, error) {
	if oldText == "" {
		return nil, errors.New("old text cannot be empty")
	}
	if maxBytes <= 0 || int64(len(current)) > maxBytes {
		return nil, ErrEditTooLarge
	}
	if bytes.IndexByte(current, 0) >= 0 || !utf8.Valid(current) || !utf8.ValidString(oldText) || !utf8.ValidString(newText) {
		return nil, ErrEditNotText
	}
	content := strings.ReplaceAll(string(current), "\r\n", "\n")
	oldText = strings.ReplaceAll(oldText, "\r\n", "\n")
	newText = strings.ReplaceAll(newText, "\r\n", "\n")
	if oldText == newText {
		return nil, errors.New("old text and new text must differ")
	}
	count := strings.Count(content, oldText)
	if count == 0 || (count != 1 && !replaceAll) {
		return nil, ErrAmbiguousEdit
	}
	if !replaceAll {
		count = 1
	}
	if growth := len(newText) - len(oldText); growth > 0 && int64(count) > (maxBytes-int64(len(content)))/int64(growth) {
		return nil, fmt.Errorf("%w: replacement would exceed %d bytes", ErrEditTooLarge, maxBytes)
	}
	limit := 1
	if replaceAll {
		limit = -1
	}
	replacement := strings.Replace(content, oldText, newText, limit)
	if int64(len(replacement)) > maxBytes {
		return nil, ErrEditTooLarge
	}
	// The upstream fs-local sampler uses JavaScript string.slice(0, 4096),
	// which counts UTF-16 code units rather than UTF-8 bytes. A byte window
	// misses the original line endings after a Unicode-heavy prefix.
	sample := firstUTF16Units(string(current), 4096)
	crlfCount := strings.Count(sample, "\r\n")
	if crlfCount > strings.Count(sample, "\n")-crlfCount {
		if int64(strings.Count(replacement, "\n")) > maxBytes-int64(len(replacement)) {
			return nil, ErrEditTooLarge
		}
		replacement = strings.ReplaceAll(replacement, "\n", "\r\n")
	}
	return []byte(replacement), nil
}

func firstUTF16Units(content string, limit int) string {
	units := 0
	for offset, character := range content {
		width := 1
		if character > 0xffff {
			width = 2
		}
		if units+width > limit {
			return content[:offset]
		}
		units += width
	}
	return content
}
