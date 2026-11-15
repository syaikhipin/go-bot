//go:build !webui

package frontend

import "io/fs"

func Files() fs.FS { return nil }
