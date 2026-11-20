package controller

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	pluginassets "go-bot/extensions/plugins"
)

// A content version prevents native image caches from retaining an old icon
// after a deployment replaces the resource named by plugin.json.
func pluginIconURL(path string) string {
	data, err := pluginassets.Asset(path)
	if err != nil {
		return path
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%s?v=%x", path, digest[:8])
}

func servePluginAsset(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, pluginassets.AssetPrefix) {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return true
	}
	data, err := pluginassets.Asset(r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, path.Base(r.URL.Path), time.Time{}, bytes.NewReader(data))
	return true
}
