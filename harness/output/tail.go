package output

import (
	"strings"
	"unicode/utf8"
)

// FitTail preserves a Job's final status after truncation, counting UTF-8
// bytes and avoiding broken multibyte characters at the retained boundary.
func FitTail(text, suffix string, limit int) string {
	if len(text)+len(suffix) <= limit {
		return text + suffix
	}
	notice := "\n[output truncated]"
	if strings.HasSuffix(text, strings.TrimSpace(notice)) {
		notice = ""
	}
	fixed := notice + suffix
	if len(fixed) >= limit {
		return tailBytes(fixed, limit)
	}
	return tailBytes(text, limit-len(fixed)) + fixed
}
func tailBytes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	index := len(text) - limit
	for index < len(text) && !utf8.RuneStart(text[index]) {
		index++
	}
	return text[index:]
}
