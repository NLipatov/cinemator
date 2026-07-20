package torrent

import (
	"os"
	"path/filepath"
	"testing"

	"cinemator/presentation/settings"
)

func TestManagerCloseReleasesCacheOwnership(t *testing.T) {
	root := t.TempDir()
	hls := filepath.Join(root, "hls")
	download := filepath.Join(root, "download")
	t.Setenv("CINEMATOR_HLS_PATH", hls)
	t.Setenv("CINEMATOR_DOWNLOAD_PATH", download)
	t.Setenv("CINEMATOR_TORRENT_PORT", "0")
	t.Setenv("CINEMATOR_MIN_FREE_BYTES", "0")
	t.Setenv("CINEMATOR_MIN_FREE_INODES", "0")
	manager, err := NewManager(settings.NewSettings())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	ownership, err := acquireCacheOwnership(hls, download)
	if err != nil {
		t.Fatalf("cache ownership remains after manager close: %v", err)
	}
	if err := ownership.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerRetainsCacheFenceWhenAssetCloseIsOutstanding(t *testing.T) {
	root := t.TempDir()
	hls := filepath.Join(root, "hls")
	download := filepath.Join(root, "download")
	t.Setenv("CINEMATOR_HLS_PATH", hls)
	t.Setenv("CINEMATOR_DOWNLOAD_PATH", download)
	t.Setenv("CINEMATOR_TORRENT_PORT", "0")
	t.Setenv("CINEMATOR_MIN_FREE_BYTES", "0")
	t.Setenv("CINEMATOR_MIN_FREE_INODES", "0")
	managerAPI, err := NewManager(settings.NewSettings())
	if err != nil {
		t.Fatal(err)
	}
	manager := managerAPI.(*manager)
	assetPath := filepath.Join(hls, "held", "chunk.ts")
	if err := os.MkdirAll(filepath.Dir(assetPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assetPath, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	asset, err := manager.media.assets.Open(assetPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err == nil {
		t.Fatal("manager released ownership with an outstanding asset handle")
	}
	if owner, err := acquireCacheOwnership(hls, download); err == nil {
		_ = owner.Close()
		t.Fatal("cache ownership fence was released while the asset remained open")
	}
	if err := asset.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.ownership.Close(); err != nil {
		t.Fatal(err)
	}
}
