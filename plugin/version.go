package plugin

import (
	"golang.org/x/mod/semver"
	"strings"
)

// NewerVersion reports a valid, strictly newer reviewed release. Installation
// state is supplied by the Host; saved Redis intent is not proof of installation.
func NewerVersion(installed, available string) string {
	a, b := "v"+strings.TrimPrefix(installed, "v"), "v"+strings.TrimPrefix(available, "v")
	if !semver.IsValid(a) || !semver.IsValid(b) || semver.Compare(b, a) <= 0 {
		return ""
	}
	return available
}
