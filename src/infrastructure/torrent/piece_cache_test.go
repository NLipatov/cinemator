package torrent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNewPieceCacheTrimsExistingDataToCapacity(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, pieceCacheDirName)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(cacheDir, "old-piece")
	if err := os.WriteFile(oversized, make([]byte, 2048), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := newPieceCache(root, newCacheBudget(1024), nil); err != nil {
		t.Fatalf("newPieceCache() error = %v", err)
	}
	if _, err := os.Stat(oversized); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized existing cache entry was not trimmed: %v", err)
	}
}

func TestValidatePieceCacheCapacityRequiresPromotionHeadroom(t *testing.T) {
	if err := validatePieceCacheCapacity(2<<20, 1<<20); err == nil {
		t.Fatal("validatePieceCacheCapacity() error = nil")
	}
	if err := validatePieceCacheCapacity(1<<20, 2<<20); err != nil {
		t.Fatalf("validatePieceCacheCapacity() error = %v", err)
	}
}

func TestNewPieceCacheRejectsSymlinkRoot(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, pieceCacheDirName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := newPieceCache(root, newCacheBudget(1024), nil); err == nil {
		t.Fatal("symbolic-link piece cache root was accepted")
	}
}
