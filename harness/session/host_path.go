package sessionformat

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// ProjectKey reproduces DeepSeek JSONL persistence's readable directory key
// for a Session header's initial cwd. JavaScript iterates UTF-16 code units,
// so astral characters become two ~XXXX escapes, not one Go rune escape.
func ProjectKey(cwd string) (string, error) {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return "", errors.New("Session project cwd must be absolute")
	}
	var readable strings.Builder
	separatorRun := false
	for _, code := range utf16.Encode([]rune(cwd)) {
		ch := byte(code)
		switch {
		case code == '/' || code == '\\' || code == ':':
			if !separatorRun {
				readable.WriteByte('-')
			}
			separatorRun = true
		case code != '~' && (code >= 'A' && code <= 'Z' || code >= 'a' && code <= 'z' || code >= '0' && code <= '9' || code == '.' || code == '_' || code == '-'):
			readable.WriteByte(ch)
			separatorRun = false
		default:
			readable.WriteString(fmt.Sprintf("~%04X", code))
			separatorRun = false
		}
	}
	slug := strings.TrimLeft(readable.String(), "-")
	if slug == "" {
		slug = "root"
	}
	if len(slug) > 251 {
		slug = slug[:251]
	}
	return "--" + slug + "--", nil
}
