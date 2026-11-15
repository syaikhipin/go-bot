package httpapi

import (
	"errors"
	"regexp"
	"time"

	"go-harness/harness/schedule"
)

var clientTimeZoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(?:/[A-Za-z0-9_+.-]+)+$`)

// A browser can provide its IANA zone with the same message request. Persist
// the Intl-canonical name, which time-context will read in another Agent step
// or after a cold restore. Absence is valid for headless clients.
func canonicalClientTimeZone(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(value) > 128 || (value != "UTC" && !clientTimeZoneName.MatchString(value)) {
		return "", errors.New("client time zone must be UTC or an IANA Area/Location")
	}
	canonical, err := schedule.CanonicalTimeZone(value)
	if err != nil {
		return "", errors.New("unsupported client time zone")
	}
	if _, err := time.LoadLocation(canonical); err != nil {
		return "", errors.New("client time zone has no available zone rules")
	}
	return canonical, nil
}
