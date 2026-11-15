package web

import (
	"strings"
	"unicode"
)

// IsSpace matches the whitespace used by the upstream JavaScript trim and \s.
func IsSpace(r rune) bool {
	return r == '\uFEFF' || r != '\u0085' && unicode.IsSpace(r)
}

func CollapseSpace(value string) string {
	return strings.Join(strings.FieldsFunc(value, IsSpace), " ")
}
