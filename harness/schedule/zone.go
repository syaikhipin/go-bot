package schedule

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"
)

var ianaZoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(?:/[A-Za-z0-9_+.-]+)+$`)

//go:embed zone_aliases.txt
var zoneAliasData string

var zoneAliases = func() map[string]string {
	aliases := make(map[string]string)
	for line := range strings.SplitSeq(zoneAliasData, "\n") {
		if alias, target, ok := strings.Cut(line, "\t"); ok {
			aliases[alias] = target
		}
	}
	return aliases
}()

// CanonicalTimeZone returns UTC or the IANA name whose rules Go can load.
func CanonicalTimeZone(value string) (string, error) {
	_, name, err := parseZone(value)
	return name, err
}

func parseZone(value string) (*time.Location, string, error) {
	if value != "UTC" && !ianaZoneName.MatchString(value) {
		return nil, "", errors.New("time_zone must name an IANA zone")
	}
	name := value
	if target, ok := zoneAliases[value]; ok {
		name = target
	}
	switch name {
	case "UTC", "Etc/UTC", "Etc/GMT":
		return time.UTC, "UTC", nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, "", fmt.Errorf("invalid time_zone %q: %w", value, err)
	}
	return loc, name, nil
}
