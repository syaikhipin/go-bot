package plugins

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func testRuntimeConfig(t *testing.T) RuntimeConfig {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the Cordis integration test")
	}
	version, _ := exec.Command(node, "--version").Output()
	var major, minor int
	_, _ = fmt.Sscanf(string(version), "v%d.%d", &major, &minor)
	if major < 24 || major == 24 && minor < 9 {
		t.Skip("Cordis Loader requires Node >=24.9")
	}
	main, err := filepath.Abs("../../extensions/cordis/src/main.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(main), "../node_modules/@deepseek-ai/cordis/package.json")); err != nil {
		t.Skip("Run pnpm install in extensions/cordis to test the actual Node runtime")
	}
	home := t.TempDir()
	return RuntimeConfig{Node: node, Main: main, ProfileDir: t.TempDir(), Home: home, DSHHome: filepath.Join(home, ".dsh"), ReservedTools: []string{"shellexecute"}}
}
