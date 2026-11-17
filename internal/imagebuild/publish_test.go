package imagebuild

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishAMD64(t *testing.T) {
	dir := t.TempDir()
	const image = "registry.example.com/agent/host:v1"
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const docker = `#!/bin/sh
set -eu
[ "${MODEL_API_KEY+x}${WEB_SEARCH_API_KEY+x}${ALIBABA_CLOUD_ACCESS_KEY_SECRET+x}${REGISTRY_PASSWORD+x}" = '' ]
[ "$DOCKER_CONFIG" = '/docker-test-config' ]
if [ "$1" = buildx ]; then
  [ "$*" = 'buildx build --platform linux/amd64 --provenance=false --load --file build/Dockerfile.host --tag go-bot/host:eci --tag registry.example.com/agent/host:v1 .' ]
  touch "$DOCKER_BUILD_MARKER"
  printf '%s\n' 'built fixture image' >&2
else
  [ "$*" = 'image push registry.example.com/agent/host:v1' ]
  [ -f "$DOCKER_BUILD_MARKER" ]
  printf '%s\n' 'v1: digest: ` + digest + ` size: 123'
fi
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", "/docker-test-config")
	t.Setenv("DOCKER_BUILD_MARKER", filepath.Join(dir, "built"))
	for _, key := range []string{"MODEL_API_KEY", "WEB_SEARCH_API_KEY", "ALIBABA_CLOUD_ACCESS_KEY_SECRET", "REGISTRY_PASSWORD"} {
		t.Setenv(key, "fixture-value")
	}
	var logs bytes.Buffer
	got, err := PublishAMD64(t.Context(), "go-bot/host:eci", image, &logs)
	if err != nil || got != digest || !strings.Contains(logs.String(), "built fixture image") {
		t.Fatalf("PublishAMD64 = %q, %v; logs = %q", got, err, logs.String())
	}
}
