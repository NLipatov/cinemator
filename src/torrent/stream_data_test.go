package torrent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cinemator/config"

	torrentlib "github.com/anacrolix/torrent"
)

func TestStreamKeyDirNameAndParse(t *testing.T) {
	key := streamKey{InfoHash: "abc123", Index: 7, Audio: 1, Subtitle: -1}

	gotName := key.dirName()
	if gotName != "abc123_7_a1_s-1" {
		t.Fatalf("dirName() = %q", gotName)
	}

	gotKey, err := parseStreamDir(gotName)
	if err != nil {
		t.Fatalf("parseStreamDir() error = %v", err)
	}
	if gotKey != key {
		t.Fatalf("parseStreamDir() = %#v, want %#v", gotKey, key)
	}
}

func TestStreamKeyPaths(t *testing.T) {
	root := t.TempDir()
	key := streamKey{InfoHash: "hash", Index: 2, Audio: 0, Subtitle: 3}

	got := key.paths(root)
	wantDir := filepath.Join(root, "hash_2_a0_s3")
	if got.outDir != wantDir {
		t.Fatalf("outDir = %q, want %q", got.outDir, wantDir)
	}
	if got.videoPlaylist != filepath.Join(wantDir, "index.m3u8") {
		t.Fatalf("videoPlaylist = %q", got.videoPlaylist)
	}
	if got.masterPlaylist != filepath.Join(wantDir, "master.m3u8") {
		t.Fatalf("masterPlaylist = %q", got.masterPlaylist)
	}
	if got.selectionMaster(1, 3) != filepath.Join(wantDir, "master_a1_s3.m3u8") {
		t.Fatalf("selectionMaster = %q", got.selectionMaster(1, 3))
	}
	if got.readyMarker != filepath.Join(wantDir, ".ready") {
		t.Fatalf("readyMarker = %q", got.readyMarker)
	}
}

func TestCompletedStreamOutputCanBeReopened(t *testing.T) {
	root := t.TempDir()
	key := streamKey{InfoHash: "hash", Index: 2, Audio: 0, Subtitle: -1}
	paths := key.paths(root)
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.masterPlaylist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ready, err := streamOutputReady(paths)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("stream without completion marker reported ready")
	}
	m := &Manager{
		active:    make(map[streamKey]*streamInfo),
		streamOps: make(map[streamKey]chan struct{}),
	}
	activated, err := m.activateCachedStream(context.Background(), key, paths)
	if err != nil || activated {
		t.Fatalf("activateCachedStream() = %v, %v; want false, nil", activated, err)
	}
	if err := os.WriteFile(paths.readyMarker, []byte("1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ready, err = streamOutputReady(paths)
	if err != nil || ready {
		t.Fatalf("legacy streamOutputReady() = %v, %v; want false, nil", ready, err)
	}
	if err := markStreamOutputReady(paths); err != nil {
		t.Fatal(err)
	}
	ready, err = streamOutputReady(paths)
	if err != nil || !ready {
		t.Fatalf("streamOutputReady() = %v, %v; want true, nil", ready, err)
	}

	activated, err = m.activateCachedStream(context.Background(), key, paths)
	if err != nil || !activated {
		t.Fatalf("activateCachedStream() = %v, %v; want true, nil", activated, err)
	}
	s := m.active[key]
	if s == nil || !s.completed || s.source != nil || s.torrent != nil {
		t.Fatalf("cached stream = %#v, want completed stream without torrent source", s)
	}
}

func TestCleanupPreservesCompletedStreamOutput(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	paths := key.paths(t.TempDir())
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.masterPlaylist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := markStreamOutputReady(paths); err != nil {
		t.Fatal(err)
	}
	s := &streamInfo{paths: paths, lastView: time.Now(), completed: true}
	m := &Manager{
		active:    map[streamKey]*streamInfo{key: s},
		streamOps: make(map[streamKey]chan struct{}),
	}
	if err := m.cleanup(context.Background(), key); err != nil {
		t.Fatalf("cleanup() error = %v", err)
	}
	ready, err := streamOutputReady(paths)
	if err != nil || !ready {
		t.Fatalf("completed output after cleanup = %v, %v; want ready", ready, err)
	}
	if m.active[key] != nil {
		t.Fatal("cleanup() left completed stream active")
	}
}

func TestCleanupCachedStreamDoesNotReleaseTorrentReference(t *testing.T) {
	id := strings.Repeat("d", 40)
	key := streamKey{InfoHash: id, Index: 1, Audio: 0, Subtitle: -1}
	paths := key.paths(t.TempDir())
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.masterPlaylist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := markStreamOutputReady(paths); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		active: map[streamKey]*streamInfo{
			key: {paths: paths, lastView: time.Now(), completed: true},
		},
		streamOps: make(map[streamKey]chan struct{}),
		torrents:  map[string]int{id: 1},
	}
	if err := m.cleanup(context.Background(), key); err != nil {
		t.Fatalf("cleanup() error = %v", err)
	}

	m.mu.Lock()
	refs := m.torrents[id]
	m.mu.Unlock()
	if refs != 1 {
		t.Fatalf("torrent refs after cached cleanup = %d, want 1", refs)
	}
}

func TestParseStreamDirRejectsMalformedNames(t *testing.T) {
	cases := []string{
		"",
		"hash_1_a0",
		"hash_x_a0_s0",
		"hash_1_x0_s0",
		"hash_1_a0_x0",
		"hash_1_a0_sx",
	}

	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseStreamDir(name); err == nil {
				t.Fatalf("parseStreamDir(%q) succeeded, want error", name)
			}
		})
	}
}

func TestStreamInfoWaitPlayableReturnsSignalError(t *testing.T) {
	want := errors.New("probe failed")
	s := &streamInfo{playable: make(chan struct{})}
	s.signalPlayable(want)

	if got := s.waitPlayable(context.Background()); !errors.Is(got, want) {
		t.Fatalf("waitPlayable() = %v, want %v", got, want)
	}
}

func TestStreamInfoWaitPlayableHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()

	s := &streamInfo{playable: make(chan struct{})}

	if err := s.waitPlayable(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitPlayable() = %v, want deadline exceeded", err)
	}
}

func TestStreamInfoSignalsPlayableOnce(t *testing.T) {
	s := &streamInfo{playable: make(chan struct{})}
	if ok := s.signalPlayable(nil); !ok {
		t.Fatal("signalPlayable() rejected first signal")
	}
	if ok := s.signalPlayable(errors.New("late failure")); ok {
		t.Fatal("signalPlayable() accepted a second signal")
	}
	if err := s.waitPlayable(context.Background()); err != nil {
		t.Fatalf("waitPlayable() = %v, want nil", err)
	}
}

func TestFinishConversionIgnoresReplacedStream(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	canceled := false
	stale := &streamInfo{cancel: func() { canceled = true }}
	current := &streamInfo{}

	m := &Manager{
		active: map[streamKey]*streamInfo{key: current},
	}
	m.finishConversion(key, stale, nil)

	if m.active[key] != current || stale.completed || canceled {
		t.Fatal("finishConversion() changed the replacement stream")
	}
}

func TestFinishConversionMarksSuccessfulRunCompleted(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	s := &streamInfo{}
	events := newDownloadEventBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := events.subscribe(ctx)

	m := &Manager{
		active: map[streamKey]*streamInfo{key: s},
		events: events,
	}
	m.finishConversion(key, s, nil)

	if !s.completed {
		t.Fatal("finishConversion() did not mark successful run as completed")
	}
	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatal("finishConversion() did not notify download subscribers")
	}
}

func TestFinishConversionCleansFailedBackgroundPreparation(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	paths := key.paths(t.TempDir())
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	s := &streamInfo{paths: paths, runDone: make(chan struct{})}
	close(s.runDone)
	m := &Manager{
		active:    map[streamKey]*streamInfo{key: s},
		streamOps: make(map[streamKey]chan struct{}),
	}

	m.finishConversion(key, s, fmt.Errorf("ffmpeg canceled: %w", context.Canceled))

	if m.active[key] != nil {
		t.Fatal("finishConversion() left failed preparation active")
	}
	if _, err := os.Stat(paths.outDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed HLS output still exists: %v", err)
	}
}

func TestGetStreamDoesNotResetCompletedHLS(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	paths := key.paths(t.TempDir())
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(paths.outDir, "chunk_00001.ts")
	if err := os.WriteFile(sentinel, []byte("cached"), 0644); err != nil {
		t.Fatal(err)
	}

	s := &streamInfo{
		paths:     paths,
		completed: true,
	}
	m := &Manager{
		active: map[streamKey]*streamInfo{key: s},
	}

	got, err := m.getStream(context.Background(), key)
	if err != nil || got != s {
		t.Fatalf("getStream() = stream %p, error %v", got, err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("getStream() removed completed HLS cache: %v", err)
	}
}

func TestGetStreamWaitsForReservedStreamOperation(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	s := &streamInfo{
		completed: true,
		paths:     key.paths(t.TempDir()),
	}
	m := &Manager{active: map[streamKey]*streamInfo{key: s}}

	m.mu.Lock()
	operationDone := m.reserveStreamOperationLocked(key)
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observedCtx := &doneObservedContext{
		Context:   ctx,
		requested: make(chan struct{}),
	}
	type result struct {
		stream *streamInfo
		exists bool
		err    error
	}
	resultReady := make(chan result, 1)
	go func() {
		got, err := m.getStream(observedCtx, key)
		resultReady <- result{stream: got, exists: got != nil, err: err}
	}()

	select {
	case <-observedCtx.requested:
	case <-time.After(time.Second):
		t.Fatal("getStream() did not wait for the reserved stream operation")
	}
	select {
	case <-resultReady:
		t.Fatal("getStream() passed the reserved stream operation")
	default:
	}

	m.finishStreamOperation(key, operationDone)
	select {
	case got := <-resultReady:
		if got.err != nil || !got.exists || got.stream != s {
			t.Fatalf("getStream() = stream %p, exists %v, error %v", got.stream, got.exists, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("getStream() did not proceed after the stream operation finished")
	}
}

func TestCanceledStartupWaiterKeepsBackgroundPreparation(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	paths := key.paths(t.TempDir())
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	partialChunk := filepath.Join(paths.outDir, "chunk_00001.ts")
	if err := os.WriteFile(partialChunk, []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}
	canceled := false
	s := &streamInfo{
		paths:    paths,
		playable: make(chan struct{}),
		runDone:  make(chan struct{}),
	}
	s.cancel = func() {
		canceled = true
		close(s.runDone)
	}
	m := &Manager{active: map[streamKey]*streamInfo{key: s}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := m.waitForPlayableStream(ctx, s)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForPlayableStream() error = %v, want %v", err, context.Canceled)
	}
	if canceled {
		t.Fatal("waitForPlayableStream() canceled background preparation")
	}
	if m.active[key] != s {
		t.Fatal("waitForPlayableStream() removed background preparation")
	}
	if _, err := os.Stat(partialChunk); err != nil {
		t.Fatalf("partial HLS chunk was removed: %v", err)
	}
}

func TestInactiveBackgroundPreparationKeepsPartialHLS(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	paths := key.paths(t.TempDir())
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	partialChunk := filepath.Join(paths.outDir, "chunk_00001.ts")
	if err := os.WriteFile(partialChunk, []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}
	canceled := false
	s := &streamInfo{
		cancel:   func() { canceled = true },
		lastView: time.Now().Add(-time.Hour),
		paths:    paths,
	}
	m := &Manager{
		active: map[streamKey]*streamInfo{key: s},
		cfg:    config.Config{ViewerTimeout: time.Minute},
	}

	m.cleanupInactiveCompletedStreams(time.Now())

	if canceled {
		t.Fatal("viewer timeout canceled background preparation")
	}
	if m.active[key] != s {
		t.Fatal("viewer timeout removed background preparation")
	}
	if _, err := os.Stat(partialChunk); err != nil {
		t.Fatalf("viewer timeout removed partial HLS: %v", err)
	}
}

func TestInactiveCompletedStreamDeactivatesButKeepsReadyHLS(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	paths := key.paths(t.TempDir())
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.masterPlaylist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := markStreamOutputReady(paths); err != nil {
		t.Fatal(err)
	}
	s := &streamInfo{
		completed: true,
		lastView:  time.Now().Add(-time.Hour),
		paths:     paths,
	}
	m := &Manager{
		active:    map[streamKey]*streamInfo{key: s},
		streamOps: make(map[streamKey]chan struct{}),
		cfg:       config.Config{ViewerTimeout: time.Minute},
	}

	m.cleanupInactiveCompletedStreams(time.Now())

	if m.active[key] != nil {
		t.Fatal("viewer timeout left completed stream active")
	}
	ready, err := streamOutputReady(paths)
	if err != nil || !ready {
		t.Fatalf("completed HLS after viewer timeout = %v, %v; want ready", ready, err)
	}
}

func TestCleanupIfCurrentIgnoresReplacement(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	stale := &streamInfo{}
	current := &streamInfo{}

	m := &Manager{
		active: map[streamKey]*streamInfo{key: current},
	}
	m.cleanupIfCurrent(key, stale)

	if m.active[key] != current {
		t.Fatal("cleanupIfCurrent() removed the replacement stream")
	}
}

func TestCleanupSerializesReplacementUntilConversionStops(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	paths := key.paths(t.TempDir())
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}

	canceled := make(chan struct{})
	runDone := make(chan struct{})
	s := &streamInfo{
		cancel:  func() { close(canceled) },
		paths:   paths,
		runDone: runDone,
	}
	m := &Manager{active: map[streamKey]*streamInfo{key: s}}
	cleanupDone := make(chan struct{})
	go func() {
		_ = m.cleanup(context.Background(), key)
		close(cleanupDone)
	}()

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("cleanup() did not cancel the conversion run")
	}
	select {
	case <-cleanupDone:
		t.Fatal("cleanup() returned before the conversion run stopped")
	case <-time.After(50 * time.Millisecond):
	}

	m.mu.Lock()
	cleanupBarrier := m.streamOps[key]
	m.mu.Unlock()
	if cleanupBarrier == nil {
		close(runDone)
		<-cleanupDone
		t.Fatal("cleanup did not reserve the stream key")
	}

	replacementPath := filepath.Join(paths.outDir, "replacement.m3u8")
	replacementReady := make(chan error, 1)
	go func() {
		if err := waitForDone(context.Background(), cleanupBarrier); err != nil {
			replacementReady <- err
			return
		}
		if err := os.MkdirAll(paths.outDir, 0755); err != nil {
			replacementReady <- err
			return
		}
		replacementReady <- os.WriteFile(replacementPath, []byte("replacement"), 0644)
	}()
	select {
	case err := <-replacementReady:
		close(runDone)
		<-cleanupDone
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("replacement passed the cleanup barrier before cleanup finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(runDone)
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup() did not finish after the conversion run stopped")
	}
	select {
	case err := <-replacementReady:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement did not start after cleanup finished")
	}
	if _, err := os.Stat(replacementPath); err != nil {
		t.Fatalf("replacement output was removed by the previous cleanup: %v", err)
	}
}

func TestCleanupDoesNotBlockDifferentStreamKey(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	differentKey := streamKey{InfoHash: "hash", Index: 1, Audio: 1, Subtitle: -1}
	ownedTorrent := new(torrentlib.Torrent)
	canceled := make(chan struct{})
	runDone := make(chan struct{})
	s := &streamInfo{
		cancel:  func() { close(canceled) },
		runDone: runDone,
		torrent: ownedTorrent,
	}
	m := &Manager{
		active:   map[streamKey]*streamInfo{key: s},
		torrents: map[string]int{key.InfoHash: 1},
	}
	cleanupDone := make(chan struct{})
	go func() {
		_ = m.cleanup(context.Background(), key)
		close(cleanupDone)
	}()

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("cleanup() did not cancel the conversion run")
	}

	differentStreamReady := make(chan struct{})
	go func() {
		m.mu.Lock()
		m.active[differentKey] = &streamInfo{torrent: ownedTorrent}
		m.torrents[differentKey.InfoHash]++
		m.mu.Unlock()
		close(differentStreamReady)
	}()
	select {
	case <-differentStreamReady:
	case <-time.After(50 * time.Millisecond):
		close(runDone)
		<-cleanupDone
		t.Fatal("cleanup blocked a different stream key")
	}

	close(runDone)
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup() did not finish after the conversion run stopped")
	}
	m.mu.Lock()
	remainingTorrentReferences := m.torrents[key.InfoHash]
	m.mu.Unlock()
	if remainingTorrentReferences != 1 {
		t.Fatalf("torrent references after cleanup = %d, want 1", remainingTorrentReferences)
	}
}

func TestResetStreamOutputRemovesStaleHLSFiles(t *testing.T) {
	paths := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}.paths(t.TempDir())
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	staleFiles := []string{
		paths.masterPlaylist,
		paths.videoPlaylist,
		filepath.Join(paths.outDir, "subs_0.m3u8"),
		paths.readyMarker,
		filepath.Join(paths.outDir, "chunk_00001.ts"),
		filepath.Join(paths.outDir, "subs_00001.vtt"),
	}
	for _, path := range staleFiles {
		if err := os.WriteFile(path, []byte("stale"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if err := resetStreamOutput(paths); err != nil {
		t.Fatalf("resetStreamOutput() error = %v", err)
	}
	if info, err := os.Stat(paths.outDir); err != nil || !info.IsDir() {
		t.Fatalf("outDir not recreated: info=%v err=%v", info, err)
	}
	for _, path := range staleFiles {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale file %s still exists: %v", path, err)
		}
	}
}

func TestTorrentStatusHasActiveWebseedRequests(t *testing.T) {
	const status = `first
Infohash: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
webseeds:
- https://example.test/first/
  active requests: 2 of [0-8)

second
Infohash: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
webseeds:
- https://example.test/second/
`

	if !torrentStatusHasActiveWebseedRequests(status, strings.Repeat("a", 40)) {
		t.Fatal("active webseed request was not detected")
	}
	if torrentStatusHasActiveWebseedRequests(status, strings.Repeat("b", 40)) {
		t.Fatal("inactive torrent reported an active webseed request")
	}
	if torrentStatusHasActiveWebseedRequests(status, strings.Repeat("c", 40)) {
		t.Fatal("missing torrent reported an active webseed request")
	}
}
