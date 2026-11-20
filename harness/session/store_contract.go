package sessionformat

import (
	"context"
	"errors"
	"regexp"
)

// OwnerClaim keeps one active writer for a Session journal.
type OwnerClaim func(context.Context, string) (release func(), err error)

var ErrV4UncertainWrite = errors.New("Session v4 append outcome is uncertain; reopen before continuing")

// ErrV4WriteRolledBack means the failed batch was removed and fsynced at the
// previous file size. The same in-memory suffix can be retried on this writer.
var ErrV4WriteRolledBack = errors.New("Session v4 append was rolled back; retry the checkpoint")

var v4KeyComponent = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validateSessionProject(projectCWD string, header Header) error {
	if projectCWD != "" && header.CWD != projectCWD {
		return errors.New("Session v4 header cwd differs from its project directory")
	}
	// Lineage is Session metadata, not a filesystem mode. Programmatic creates
	// may seed/fork or name a parent independently of the built-in subagent
	// coordinator. UI listing and coordinator admission own their own checks.
	return nil
}
