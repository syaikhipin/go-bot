//go:build webui

package frontend

import (
	"embed"
	"io/fs"
)

//go:embed dist
var bundle embed.FS

func Files() fs.FS {
	files, err := fs.Sub(bundle, "dist")
	if err != nil {
		panic(err)
	}
	return files
}
