package torrent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cinemator/presentation/settings"

	torrentlib "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
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
	cleanupStarted := make(chan struct{})
	runDone := make(chan struct{})
	var stopRunOnce sync.Once
	stopRun := func() { stopRunOnce.Do(func() { close(runDone) }) }
	defer stopRun()
	s := &streamInfo{
		cancel:  func() { close(cleanupStarted) },
		paths:   key.paths(filepath.Join(root, "hls")),
		runDone: runDone,
		running: true,
	}
	m := &manager{
		client:    client,
		active:    map[streamKey]*streamInfo{key: s},
		streamOps: make(map[streamKey]chan struct{}),
		downloads: downloads,
		settings:  settings.NewSettings(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- m.DeleteDownload(ctx, id) }()

	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		stopRun()
		t.Fatal("DeleteDownload() did not start stream cleanup")
	}
	cancel()

	var deleteErr error
	select {
	case deleteErr = <-result:
	case <-time.After(time.Second):
		stopRun()
		deleteErr = <-result
		t.Fatalf("DeleteDownload() ignored its context and returned only after teardown: %v", deleteErr)
	}
	stopRun()
	if !errors.Is(deleteErr, context.Canceled) {
		t.Fatalf("DeleteDownload() error = %v, want canceled", deleteErr)
	}
	m.mu.Lock()
	cleanupDone := m.streamOps[key]
	m.mu.Unlock()
	if err := waitForDone(context.Background(), cleanupDone); err != nil {
		t.Fatalf("waiting for stream cleanup: %v", err)
	}
}

func TestRetainTorrentDoesNotAddDuringConcurrentDownloadDeletion(t *testing.T) {
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
	cleanupStarted := make(chan struct{})
	runDone := make(chan struct{})
	var stopRunOnce sync.Once
	stopRun := func() { stopRunOnce.Do(func() { close(runDone) }) }
	defer stopRun()
	m := &manager{
		client: client,
		active: map[streamKey]*streamInfo{
			key: {
				cancel:  func() { close(cleanupStarted) },
				paths:   key.paths(filepath.Join(root, "hls")),
				runDone: runDone,
				running: true,
			},
		},
		streamOps: make(map[streamKey]chan struct{}),
		downloads: downloads,
		settings:  settings.NewSettings(),
	}
	deleteResult := make(chan error, 1)
	go func() { deleteResult <- m.DeleteDownload(context.Background(), id) }()
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		stopRun()
		t.Fatal("DeleteDownload() did not start stream cleanup")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := &doneObservedContext{
		Context:   ctx,
		requested: make(chan struct{}),
	}
	result := make(chan error, 1)
	go func() {
		_, _, err := m.retainTorrent(observed, "magnet:?xt=urn:btih:"+id)
		result <- err
	}()

	select {
	case <-observed.requested:
	case <-time.After(time.Second):
		t.Fatal("retainTorrent() did not wait for the deletion")
	}
	_, addedDuringDeletion := client.Torrent(metainfo.NewHashFromHex(id))
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("retainTorrent() error = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retainTorrent() did not stop after cancellation")
	}
	stopRun()
	select {
	case err := <-deleteResult:
		if err != nil {
			t.Fatalf("DeleteDownload() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DeleteDownload() did not finish after the stream stopped")
	}
	if addedDuringDeletion {
		t.Fatal("retainTorrent() added the torrent while its download was being deleted")
	}
}

func TestActivateCachedStreamWaitsForConcurrentDownloadDeletion(t *testing.T) {
	root := t.TempDir()
	id := strings.Repeat("b", 40)
	key := streamKey{InfoHash: id, Index: 0, Audio: 0, Subtitle: -1}
	paths := key.paths(filepath.Join(root, "hls"))
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.masterPlaylist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := markStreamOutputReady(paths); err != nil {
		t.Fatal(err)
	}

	deletionDone := make(chan struct{})
	m := &manager{
		active:    make(map[streamKey]*streamInfo),
		streamOps: make(map[streamKey]chan struct{}),
		deletions: map[string]chan struct{}{id: deletionDone},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := &doneObservedContext{
		Context:   ctx,
		requested: make(chan struct{}),
	}
	type result struct {
		activated bool
		err       error
	}
	resultReady := make(chan result, 1)
	go func() {
		activated, err := m.activateCachedStream(observed, key, paths)
		resultReady <- result{activated: activated, err: err}
	}()

	select {
	case <-observed.requested:
	case <-time.After(time.Second):
		t.Fatal("activateCachedStream() did not wait for the deletion")
	}
	select {
	case got := <-resultReady:
		t.Fatalf("activateCachedStream() passed the deletion barrier: %#v", got)
	default:
	}

	m.finishDownloadDeletion(id, deletionDone)
	select {
	case got := <-resultReady:
		if got.err != nil || !got.activated {
			t.Fatalf("activateCachedStream() = %v, %v; want true, nil", got.activated, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("activateCachedStream() did not continue after deletion finished")
	}
}

func TestDropTorrentAfterWebseedsStopHonorsCanceledContext(t *testing.T) {
	client, err := torrentlib.NewClient(torrentlib.TestingConfig(t))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { client.Close() })
	id := strings.Repeat("c", 40)
	tor, err := client.AddMagnet("magnet:?xt=urn:btih:" + id)
	if err != nil {
		t.Fatalf("AddMagnet() error = %v", err)
	}
	m := &manager{client: client}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = m.dropTorrentAfterWebseedsStop(ctx, tor)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dropTorrentAfterWebseedsStop() error = %v, want context canceled", err)
	}
	if _, exists := client.Torrent(metainfo.NewHashFromHex(id)); !exists {
		t.Fatal("dropTorrentAfterWebseedsStop() dropped torrent after context cancellation")
	}
}

type doneObservedContext struct {
	context.Context
	once      sync.Once
	requested chan struct{}
}

func (c *doneObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.requested) })
	return c.Context.Done()
}
