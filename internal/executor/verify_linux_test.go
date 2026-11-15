//go:build linux

package executor

import (
	"os"
	"sync"
	"testing"
)

func TestHostMountInfoSeparatesContainerAndMountedModes(t *testing.T) {
	base := "1 0 0:1 / / rw - overlay overlay rw\n"
	mapped := base + "3 1 0:3 / /root/harness rw,nosuid - nfs nas rw\n"
	if err := verifyHostMountInfo(base, "container"); err != nil {
		t.Fatalf("container layer rejected: %v", err)
	}
	if err := verifyHostMountInfo(mapped, "mounted"); err != nil {
		t.Fatalf("NAS user mount rejected: %v", err)
	}
	if err := verifyHostMountInfo(base+"3 1 0:3 /volumes/alice/_data /root/harness rw - ext4 /dev/vda rw\n", "mounted"); err != nil {
		t.Fatalf("Docker volume rejected: %v", err)
	}
	for _, test := range []struct{ data, mode string }{
		{base, "mounted"},
		{mapped, "container"},
		{base + "3 1 0:3 / /root/harness ro - nfs nas rw\n", "mounted"},
	} {
		if err := verifyHostMountInfo(test.data, test.mode); err == nil {
			t.Fatalf("accepted invalid %s mounts: %q", test.mode, test.data)
		}
	}
}

func TestHostFileSemanticsProbeChecksAndCleansConcurrentOperations(t *testing.T) {
	root := t.TempDir()
	var workers sync.WaitGroup
	errors := make(chan error, 8)
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := probeHostFileSemantics(root); err != nil {
				errors <- err
			}
		}()
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("filesystem capability probe left files behind: %v, %v", entries, err)
	}
}
