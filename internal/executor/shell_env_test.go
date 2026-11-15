package executor

import (
	"strings"
	"testing"
)

func TestShellEnvironmentInheritsContainerEnvironment(t *testing.T) {
	t.Setenv("HOME", "/root")
	t.Setenv("NPM_CONFIG_PREFIX", "/root/.local")
	t.Setenv("GOPATH", "/root/go")
	t.Setenv("GOCACHE", "/root/.cache/go-build")
	t.Setenv("SERVERAGENT_OSS_ACCESS_KEY_ID", "oss-key-visible-to-shell")
	t.Setenv("SERVERAGENT_OSS_ACCESS_KEY_SECRET", "oss-secret-visible-to-shell")
	values := shellEnvironment()
	joined := "\n" + strings.Join(values, "\n") + "\n"
	for _, want := range []string{
		"HOME=/root", "NPM_CONFIG_PREFIX=/root/.local",
		"GOPATH=/root/go", "GOCACHE=/root/.cache/go-build",
		"SERVERAGENT_OSS_ACCESS_KEY_ID=oss-key-visible-to-shell", "SERVERAGENT_OSS_ACCESS_KEY_SECRET=oss-secret-visible-to-shell",
	} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Fatalf("missing inherited shell environment %q in %q", want, joined)
		}
	}
}
