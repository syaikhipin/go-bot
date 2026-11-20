package plugins

import (
	"io/fs"
	"strings"
)

const AssetPrefix = "/assets/plugins/"

// Asset reads published plugin image resources without installing or loading Node.
func Asset(urlPath string) ([]byte, error) {
	rel, ok := strings.CutPrefix(urlPath, AssetPrefix)
	if !ok || !fs.ValidPath(rel) || strings.Count(rel, "/") != 1 || strings.ContainsAny(rel, "\\?#") {
		return nil, fs.ErrNotExist
	}
	id, name, _ := strings.Cut(rel, "/")
	return files.ReadFile(id + "/assets/" + name)
}
