package torrent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCacheOwnershipRejectsSecondProcessOwner(t *testing.T) {
	root := t.TempDir()
	first, err := acquireCacheOwnership(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireCacheOwnership(root); err == nil {
		t.Fatal("second cache owner was accepted")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := acquireCacheOwnership(root)
	if err != nil {
		t.Fatalf("cache was not released: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCacheOwnershipRejectsSymlinkLock(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, cacheOwnerLockName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if owner, err := acquireCacheOwnership(root); err == nil {
		_ = owner.Close()
		t.Fatal("symbolic-link cache owner lock was accepted")
	}
}

func TestValidateCacheRootsRejectsOverlap(t *testing.T) {
	root := t.TempDir()
	if err := validateCacheRoots(root, root); err == nil {
		t.Fatal("equal cache roots were accepted")
	}
	if err := validateCacheRoots(root, root+"/download"); err == nil {
		t.Fatal("nested cache roots were accepted")
	}
}

func TestValidateCacheRootsRejectsSymlinkedOverlap(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "cache")
	download := filepath.Join(realRoot, "download")
	if err := os.MkdirAll(download, 0755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := validateCacheRoots(realRoot, filepath.Join(alias, "download")); err == nil {
		t.Fatal("symlinked nested cache roots were accepted")
	}
}

func TestDiscardPreviousHlsStreamsExpiresOldProcessOutput(t *testing.T) {
	root := t.TempDir()
	old := root + "/old/chunk.ts"
	if err := os.MkdirAll(filepath.Dir(old), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := discardPreviousHlsStreams(assets, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old process output remains: %v", err)
	}
}
