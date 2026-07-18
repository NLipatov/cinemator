//go:build unix

package torrent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestChildGuardKeepsCacheFencedAfterParentDescriptorCloses(t *testing.T) {
	root := t.TempDir()
	guard, err := os.OpenFile(filepath.Join(root, cacheOwnerLockName), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockCacheOwner(guard); err != nil {
		t.Fatal(err)
	}

	started := filepath.Join(root, "started")
	cmd := exec.Command("sh", "-c", "touch \"$1\"; exec sleep 30", "sh", started)
	cmd.ExtraFiles = []*os.File{guard}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	for deadline := time.Now().Add(2 * time.Second); ; {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("guarded child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if owner, err := acquireCacheOwnership(root); err == nil {
		_ = owner.Close()
		t.Fatal("child did not retain the cache ownership fence")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed guarded child exited successfully")
	}
	cmd.Process = nil
	owner, err := acquireCacheOwnership(root)
	if err != nil {
		t.Fatalf("cache fence remained after child exit: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}
