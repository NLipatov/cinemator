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

	if _, err := newPieceCache(root, 1024); err != nil {
		t.Fatalf("newPieceCache() error = %v", err)
	}
	if _, err := os.Stat(oversized); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized existing cache entry was not trimmed: %v", err)
	}
}

func TestValidatePieceCacheCapacityRejectsPieceLargerThanCache(t *testing.T) {
	if err := validatePieceCacheCapacity(2<<20, 1<<20); err == nil {
		t.Fatal("validatePieceCacheCapacity() error = nil")
	}
	if err := validatePieceCacheCapacity(1<<20, 2<<20); err != nil {
		t.Fatalf("validatePieceCacheCapacity() error = %v", err)
	}
}
