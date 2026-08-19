package torrent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cinemator/presentation/settings"

	torrentlib "github.com/anacrolix/torrent"
)

func TestDeleteDownloadHonorsContextWhileStreamStops(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", filepath.Join(root, "hls"))
	t.Setenv("CINEMATOR_DOWNLOAD_PATH", filepath.Join(root, "downloads"))

	client, err := torrentlib.NewClient(torrentlib.TestingConfig(t))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { client.Close() })
	downloads, err := newDownloadStore(filepath.Join(root, "downloads"))
	if err != nil {
		t.Fatalf("newDownloadStore() error = %v", err)
	}

	id := strings.Repeat("a", 40)
	key := streamKey{InfoHash: id, Index: 0, Audio: 0, Subtitle: -1}
	runDone := make(chan struct{})
	s := &streamInfo{
		cancel:  func() {},
		paths:   key.paths(filepath.Join(root, "hls")),
		runDone: runDone,
		running: true,
	}
	m := &manager{
		client:    client,
		active:    map[streamKey]*streamInfo{key: s},
		streamOps: make(map[streamKey]chan struct{}),
		torrents:  map[string]int{id: 1},
		downloads: downloads,
		settings:  settings.NewSettings(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- m.DeleteDownload(ctx, id) }()

	blockedPastDeadline := false
	var deleteErr error
	select {
	case deleteErr = <-result:
	case <-time.After(250 * time.Millisecond):
		blockedPastDeadline = true
	}
	close(runDone)
	if blockedPastDeadline {
		deleteErr = <-result
	}
	if !errors.Is(deleteErr, context.DeadlineExceeded) {
		t.Fatalf("DeleteDownload() error = %v, want deadline exceeded", deleteErr)
	}
	m.mu.Lock()
	cleanupDone := m.streamOps[key]
	m.mu.Unlock()
	if err := waitForDone(context.Background(), cleanupDone); err != nil {
		t.Fatalf("waiting for stream cleanup: %v", err)
	}
	if blockedPastDeadline {
		t.Fatalf("DeleteDownload() ignored its context and returned only after teardown: %v", deleteErr)
	}
}
