//go:build linux

package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-bot/harness/permission"
	"go-bot/internal/agentfs"
)

func TestLandlockRestrictsShellAndNativeWrites(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	executor := Executor{verified: true}
	workspace := permission.WithScope(ctx, permission.Scope{Mode: permission.WorkspaceWrite, Root: root})
	readonly := permission.WithScope(ctx, permission.Scope{Mode: permission.ReadOnly, Root: root})
	probe := func(ctx context.Context, command string, pass bool) {
		t.Helper()
		t.Log(command)
		result, err := executor.Run(ctx, command, root, 65536)
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		if (result.ExitCode == 0) != pass {
			t.Fatalf("%s: exit=%d stderr=%s", command, result.ExitCode, result.Stderr)
		}
	}
	t.Run("read-only", func(t *testing.T) {
		probe(readonly, "ls . >/dev/null", true)
		probe(readonly, "printf data > read-only.txt", false)
	})
	t.Run("workspace-write", func(t *testing.T) {
		probe(workspace, "printf data > inside.txt", true)
		probe(workspace, `printf temp > "$TMPDIR"/temp.txt`, true)
		probe(workspace, "printf data > outside/escape.txt", false)
		probe(workspace, "mv inside.txt outside/escape.txt", false)
		probe(workspace, "ln inside.txt outside/escape.txt", false)
		files := agentfs.NewObservedContainerView(agentfs.ContainerView{DefaultWorkdir: root})
		if _, err := files.WriteFileContext(confineMutation(workspace), filepath.Join(outside, "native.txt"), []byte("blocked")); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("native mutation expected OS denial, got %v", err)
		}
		if _, err := files.WriteFileContext(confineMutation(workspace), "native.txt", []byte("ok")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unconfined-after-confined", func(t *testing.T) {
		probe(ctx, "printf full > outside/full.txt", true)
		for _, name := range []string{"read-only.txt", "outside/escape.txt", "outside/native.txt"} {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); !os.IsNotExist(err) {
				t.Fatalf("unexpected %s: %v", name, err)
			}
		}
		data, err := os.ReadFile(filepath.Join(outside, "full.txt"))
		if err != nil || strings.TrimSpace(string(data)) != "full" {
			t.Fatalf("full access: %q %v", data, err)
		}
	})
}
