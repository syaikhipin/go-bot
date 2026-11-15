package agentfs

import "errors"

// ErrMutationOutcomeUnknown means a file operation may have changed the
// workspace despite returning an error. The caller must inspect the current
// file state before deciding whether to retry an operation with side effects.
var ErrMutationOutcomeUnknown = errors.New("file mutation outcome is unknown")

func uncertainMutation(err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(ErrMutationOutcomeUnknown, err)
}
