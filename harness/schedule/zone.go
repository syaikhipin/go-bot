package schedule

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

var ianaZoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(?:/[A-Za-z0-9_+.-]+)+$`)
var canonicalZones sync.Map
var zoneLinks struct {
	sync.Once
	targets map[string]string
}

// CanonicalTimeZone uses the same Intl canonicalization as the current
// DeepSeek Schedule runtime. The complete Host image includes Node.js; keep
// this at the Schedule boundary so a submitted alias never changes the stored
// rule or the equality decision merely because Go's tzdata uses another name.
func CanonicalTimeZone(value string) (string, error) {
	if value == "UTC" {
		return value, nil
	}
	if !ianaZoneName.MatchString(value) {
		return "", errors.New("time_zone must name an IANA zone")
	}
	if cached, ok := canonicalZones.Load(value); ok {
		return cached.(string), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "node", "-e",
		`process.stdout.write(new Intl.DateTimeFormat('en-US',{timeZone:process.argv[1]}).resolvedOptions().timeZone)`, value).Output()
	if err != nil {
		return "", fmt.Errorf("invalid time_zone %q: %w", value, err)
	}
	canonical := string(output)
	if canonical != "UTC" && !ianaZoneName.MatchString(canonical) {
		return "", errors.New("Intl returned an invalid IANA time_zone")
	}
	canonicalZones.Store(value, canonical)
	return canonical, nil
}

// Some slim tzdata packages omit legacy alias files even though Intl accepts
// and canonicalizes those names. tzdata.zi links locate the same zone rules
// without changing the Intl spelling retained in the Schedule record.
func zoneLinkTargets() map[string]string {
	zoneLinks.Do(func() {
		zoneLinks.targets = make(map[string]string)
		data, err := os.ReadFile("/usr/share/zoneinfo/tzdata.zi")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[0] == "L" {
				zoneLinks.targets[fields[2]] = fields[1]
			}
		}
	})
	return zoneLinks.targets
}

func parseZone(value string) (*time.Location, string, error) {
	canonical, err := CanonicalTimeZone(value)
	if err != nil {
		return nil, "", err
	}
	for _, candidate := range []string{canonical, value} {
		if loc, loadErr := time.LoadLocation(candidate); loadErr == nil {
			return loc, canonical, nil
		}
	}
	links := zoneLinkTargets()
	for _, candidate := range []string{links[canonical], links[value]} {
		if candidate == "" {
			continue
		}
		if loc, loadErr := time.LoadLocation(candidate); loadErr == nil {
			return loc, canonical, nil
		}
	}
	return nil, "", fmt.Errorf("IANA time_zone %q has no available zone rules", value)
}
