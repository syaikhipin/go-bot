package goal

import (
	"errors"
	"regexp"
)

// Match the default round cap and blocker-code syntax of the current
// DeepSeek Goal service. The v4 event admission validates the same syntax.
const DefaultMaxRounds = 256

var blockCode = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

var ErrNotFound = errors.New("no current goal")
var ErrStaleRevision = errors.New("goal ID or revision is stale")
var ErrInvalidTransition = errors.New("invalid goal transition")
