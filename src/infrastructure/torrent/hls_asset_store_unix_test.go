//go:build unix

package torrent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHlsAssetStoreRejectsFifoWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "asset.ts")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		asset, openErr := store.Open(path)
		if openErr == nil {
			_ = asset.Close()
		}
		done <- openErr
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO was opened as an HLS asset")
		}
	case <-time.After(time.Second):
		_ = os.Remove(path)
		t.Fatal("opening a FIFO blocked")
	}
}
