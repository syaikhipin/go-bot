//go:build !linux && !darwin

package agentfs

import (
	"fmt"
	"os"
)

func fileVersion(info os.FileInfo) string {
	// Production runs on Linux; this fallback keeps the test-only native view
	// buildable on other developer platforms without assuming a Stat_t shape.
	return fmt.Sprintf("%d:%d:%v", info.Size(), info.ModTime().UnixNano(), info.Mode())
}
