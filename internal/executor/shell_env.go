package executor

import "os"

type Executor struct {
	verified    bool
	environment []string
}

// shellEnvironment inherits the worker container's environment. Container
// isolation, not an environment-variable allowlist, bounds Agent shell access.
func shellEnvironment() []string {
	return os.Environ()
}
