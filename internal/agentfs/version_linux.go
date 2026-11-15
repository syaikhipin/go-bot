//go:build linux

package agentfs

import (
	"fmt"
	"os"
	"syscall"
)

func fileVersion(info os.FileInfo) string {
	stat := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d:%d:%d:%d", stat.Dev, stat.Ino, info.Size(), stat.Mtim.Nano(), stat.Ctim.Nano())
}
