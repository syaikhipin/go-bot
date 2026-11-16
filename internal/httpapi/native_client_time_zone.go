package httpapi

import (
	"errors"

	"go-harness/harness/schedule"
)

// A browser can provide its IANA zone with the same message request. Persist
// the canonical IANA name, which time-context will read in another Agent step
// or after a cold restore. Absence is valid for headless clients.
func canonicalClientTimeZone(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(value) > 128 {
		return "", errors.New("client time zone must be UTC or an IANA Area/Location")
	}
	return schedule.CanonicalTimeZone(value)
}
