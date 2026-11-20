package controller

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"go-bot/extensions/catalog"
	pluginassets "go-bot/extensions/plugins"
)

func TestPluginManifestIconsAreServedWithoutAHost(t *testing.T) {
	handler := (&Manager{}).Handler()
	for _, plugin := range catalog.Entries() {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, plugin.Icon, nil))
		if res.Code != http.StatusOK || res.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("%s: %d %s", plugin.Icon, res.Code, res.Header().Get("Content-Type"))
		}
		if _, err := png.Decode(res.Body); err != nil {
			t.Fatalf("%s: invalid image: %v", plugin.Icon, err)
		}
		head := httptest.NewRecorder()
		handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, plugin.Icon, nil))
		if head.Code != http.StatusOK || head.Body.Len() != 0 {
			t.Fatalf("HEAD %s: %d", plugin.Icon, head.Code)
		}
	}
	for _, path := range []string{"/assets/plugins/missing/icon.png", "/assets/plugins/agent-browser/../package.json"} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusNotFound {
			t.Fatalf("unexpected asset %s: %d", path, res.Code)
		}
	}
}

func TestPluginTutorialVideoSupportsNativePlayerRequests(t *testing.T) {
	handler := (&Manager{}).Handler()
	count := 0
	for _, entry := range catalog.Entries() {
		for _, tutorial := range entry.Tutorials {
			count++
			t.Run(entry.ID+"/"+tutorial.Language, func(t *testing.T) {
				data, err := pluginassets.Asset(tutorial.Video)
				if err != nil {
					t.Fatal(err)
				}
				if len(data) < 32 || string(data[4:8]) != "ftyp" {
					t.Fatal("tutorial must be an actual MP4")
				}
				request := httptest.NewRequest(http.MethodGet, tutorial.Video, nil)
				request.Header.Set("Range", "bytes=0-31")
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, request)
				if res.Code != http.StatusPartialContent || !bytes.Equal(res.Body.Bytes(), data[:32]) || res.Header().Get("Content-Range") != "bytes 0-31/"+strconv.Itoa(len(data)) || res.Header().Get("Content-Type") != "video/mp4" {
					t.Fatalf("video range: %d %v", res.Code, res.Header())
				}
				head := httptest.NewRecorder()
				handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, tutorial.Video, nil))
				if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(len(data)) {
					t.Fatalf("video HEAD: %d %v", head.Code, head.Header())
				}
			})
		}
	}
	if count == 0 {
		t.Fatal("no tutorial videos exercised")
	}
}
