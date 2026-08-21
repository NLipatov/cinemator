package torrent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if got.subtitlePlaylist != filepath.Join(wantDir, "subs.m3u8") {
		t.Fatalf("subtitlePlaylist = %q", got.subtitlePlaylist)
	}
	if got.masterPlaylist != filepath.Join(wantDir, "master.m3u8") {
		t.Fatalf("masterPlaylist = %q", got.masterPlaylist)
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
	s := &streamInfo{}
	runID := s.beginRun()
	s.signalPlayable(runID, want)

	if got := s.waitPlayable(context.Background()); !errors.Is(got, want) {
		t.Fatalf("waitPlayable() = %v, want %v", got, want)
	}
}

func TestStreamInfoWaitPlayableHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()

	s := &streamInfo{}
	s.beginRun()

	if err := s.waitPlayable(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitPlayable() = %v, want deadline exceeded", err)
	}
}

func TestStreamInfoIgnoresStaleRunPlayableSignal(t *testing.T) {
	s := &streamInfo{}
	staleRunID := s.beginRun()
	currentRunID := s.beginRun()

	if ok := s.signalPlayable(staleRunID, errors.New("stale failure")); ok {
		t.Fatal("signalPlayable() accepted stale run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if err := s.waitPlayable(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitPlayable() = %v, want current run to remain pending", err)
	}

	if ok := s.signalPlayable(currentRunID, nil); !ok {
		t.Fatal("signalPlayable() rejected current run")
	}
	if err := s.waitPlayable(context.Background()); err != nil {
		t.Fatalf("waitPlayable() = %v, want nil", err)
	}
}

func TestFinishConversionIgnoresStaleRun(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	s := &streamInfo{running: true}
	staleRunID := s.beginRun()
	s.beginRun()

	m := &manager{
		active: map[streamKey]*streamInfo{key: s},
	}
	m.finishConversion(key, s, staleRunID, context.Canceled)

	if !s.running {
		t.Fatal("finishConversion() marked current run as stopped")
	}
	if s.paused {
		t.Fatal("finishConversion() marked current run as paused")
	}
}

func TestFinishConversionMarksSuccessfulRunCompleted(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	s := &streamInfo{running: true, paused: true}
	runID := s.beginRun()
	events := newDownloadEventBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := events.subscribe(ctx)

	m := &manager{
		active: map[streamKey]*streamInfo{key: s},
		events: events,
	}
	m.finishConversion(key, s, runID, nil)

	if s.running {
		t.Fatal("finishConversion() left successful run marked as running")
	}
	if s.paused {
		t.Fatal("finishConversion() left successful run marked as paused")
	}
	if !s.completed {
		t.Fatal("finishConversion() did not mark successful run as completed")
	}
	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatal("finishConversion() did not notify download subscribers")
	}
}

func TestFinishConversionTreatsWrappedCancellationAsPause(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	s := &streamInfo{running: true}
	runID := s.beginRun()
	m := &manager{active: map[streamKey]*streamInfo{key: s}}

	m.finishConversion(key, s, runID, fmt.Errorf("ffmpeg canceled: %w", context.Canceled))

	if s.running {
		t.Fatal("finishConversion() left canceled run marked as running")
	}
	if !s.paused {
		t.Fatal("finishConversion() did not mark canceled run as paused")
	}
	if m.active[key] != s {
		t.Fatal("finishConversion() removed resumable stream after cancellation")
	}
}

func TestCompletedStreamIsNotPausedOrReset(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	paths := key.paths(t.TempDir())
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(paths.outDir, "chunk_00001.ts")
	if err := os.WriteFile(sentinel, []byte("cached"), 0644); err != nil {
		t.Fatal(err)
	}

	canceled := false
	s := &streamInfo{
		cancel:    func() { canceled = true },
		paths:     paths,
		completed: true,
	}
	m := &manager{
		active: map[streamKey]*streamInfo{key: s},
	}

	m.pauseStream(key)
	if canceled {
		t.Fatal("pauseStream() canceled a completed stream")
	}
	if s.paused {
		t.Fatal("pauseStream() marked a completed stream as paused")
	}

	_, _, resumed, exists, err := m.getOrResumeStream(context.Background(), key)
	if err != nil || !exists || resumed {
		t.Fatalf("getOrResumeStream() = exists %v, resumed %v, error %v", exists, resumed, err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("getOrResumeStream() removed completed HLS cache: %v", err)
	}
}

func TestGetOrResumeStreamReturnsResetFailure(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}

	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	s := &streamInfo{
		paused: true,
		paths: streamPaths{
			outDir: filepath.Join(blocker, "stream"),
		},
	}
	m := &manager{active: map[streamKey]*streamInfo{key: s}}

	got, _, resumed, exists, err := m.getOrResumeStream(context.Background(), key)
	if !exists || got != s {
		t.Fatalf("getOrResumeStream() stream = %p, exists = %v", got, exists)
	}
	if resumed {
		t.Fatal("getOrResumeStream() reported a failed resume as successful")
	}
	if err == nil {
		t.Fatal("getOrResumeStream() error = nil, want reset failure")
	}
}

func TestResumeWaitDoesNotHoldManagerLock(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	s := &streamInfo{
		paused:  true,
		paths:   key.paths(t.TempDir()),
		runDone: make(chan struct{}),
	}
	m := &manager{active: map[streamKey]*streamInfo{key: s}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resumeResult := make(chan error, 1)
	go func() {
		_, _, _, _, err := m.getOrResumeStream(ctx, key)
		resumeResult <- err
	}()

	operationObserved := make(chan struct{})
	go func() {
		for {
			m.mu.Lock()
			waiting := m.streamOps[key] != nil
			m.mu.Unlock()
			if waiting {
				close(operationObserved)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	select {
	case <-operationObserved:
	case <-time.After(time.Second):
		t.Fatal("resume did not enter the old-run wait")
	}

	differentKeyReady := make(chan struct{})
	go func() {
		m.mu.Lock()
		m.active[streamKey{InfoHash: "other"}] = &streamInfo{}
		m.mu.Unlock()
		close(differentKeyReady)
	}()
	select {
	case <-differentKeyReady:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("resume wait blocked unrelated manager state")
	}

	cancel()
	select {
	case err := <-resumeResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("getOrResumeStream() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("getOrResumeStream() did not honor cancellation")
	}
}

func TestGetOrResumeStreamWaitsForReservedStreamOperation(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	s := &streamInfo{
		completed: true,
		paths:     key.paths(t.TempDir()),
	}
	m := &manager{active: map[streamKey]*streamInfo{key: s}}

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
		stream  *streamInfo
		resumed bool
		exists  bool
		err     error
	}
	resultReady := make(chan result, 1)
	go func() {
		got, _, resumed, exists, err := m.getOrResumeStream(observedCtx, key)
		resultReady <- result{stream: got, resumed: resumed, exists: exists, err: err}
	}()

	select {
	case <-observedCtx.requested:
	case <-time.After(time.Second):
		t.Fatal("getOrResumeStream() did not wait for the reserved stream operation")
	}
	select {
	case <-resultReady:
		t.Fatal("getOrResumeStream() passed the reserved stream operation")
	default:
	}

	m.finishStreamOperation(key, operationDone)
	select {
	case got := <-resultReady:
		if got.err != nil || !got.exists || got.resumed || got.stream != s {
			t.Fatalf("getOrResumeStream() = stream %p, exists %v, resumed %v, error %v", got.stream, got.exists, got.resumed, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("getOrResumeStream() did not proceed after the stream operation finished")
	}
}

func TestCanceledLastStartupWaiterCleansAbandonedStream(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	paths := key.paths(t.TempDir())
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	canceled := false
	s := &streamInfo{
		paths:   paths,
		running: true,
	}
	runID := s.beginRun()
	s.cancel = func() {
		canceled = true
		close(s.runDone)
	}
	s.registerStartupWaiter()
	m := &manager{active: map[streamKey]*streamInfo{key: s}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := m.waitForPlayableStream(ctx, key, s, runID)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForPlayableStream() error = %v, want %v", err, context.Canceled)
	}
	if !canceled {
		t.Fatal("waitForPlayableStream() did not cancel abandoned conversion")
	}
	if _, ok := m.active[key]; ok {
		t.Fatal("waitForPlayableStream() left abandoned stream active")
	}
	m.mu.Lock()
	cleanupDone := m.streamOps[key]
	m.mu.Unlock()
	if err := waitForDone(context.Background(), cleanupDone); err != nil {
		t.Fatalf("waiting for abandoned stream cleanup: %v", err)
	}
	if _, err := os.Stat(paths.outDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned HLS directory still exists: %v", err)
	}
}

func TestCanceledStartupWaiterKeepsStreamForAnotherWaiter(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	s := &streamInfo{paths: key.paths(t.TempDir()), running: true}
	runID := s.beginRun()
	s.registerStartupWaiter()
	s.registerStartupWaiter()
	m := &manager{active: map[streamKey]*streamInfo{key: s}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.waitForPlayableStream(ctx, key, s, runID); !errors.Is(err, context.Canceled) {
		t.Fatalf("first waitForPlayableStream() error = %v, want %v", err, context.Canceled)
	}
	if m.active[key] != s {
		t.Fatal("canceled waiter removed stream owned by another waiter")
	}

	s.signalPlayable(runID, nil)
	if _, err := m.waitForPlayableStream(context.Background(), key, s, runID); err != nil {
		t.Fatalf("second waitForPlayableStream() error = %v", err)
	}
	if m.active[key] != s {
		t.Fatal("successful waiter removed shared stream")
	}
}

func TestCanceledStartupWaiterKeepsViewerOwnedStream(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	s := &streamInfo{paths: key.paths(t.TempDir()), running: true}
	runID := s.beginRun()
	s.registerStartupWaiter()
	m := &manager{active: map[streamKey]*streamInfo{key: s}}
	m.TouchStream(context.Background(), key.dirName())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.waitForPlayableStream(ctx, key, s, runID); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForPlayableStream() error = %v, want %v", err, context.Canceled)
	}
	if m.active[key] != s {
		t.Fatal("canceled waiter removed viewer-owned stream")
	}
}

func TestCleanupIfCurrentRunIgnoresStaleRun(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	s := &streamInfo{}
	staleRunID := s.beginRun()
	s.beginRun()

	m := &manager{
		active: map[streamKey]*streamInfo{key: s},
	}
	m.cleanupIfCurrentRun(key, s, staleRunID)

	if _, ok := m.active[key]; !ok {
		t.Fatal("cleanupIfCurrentRun() removed current run for stale run ID")
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
		running: true,
	}
	m := &manager{active: map[streamKey]*streamInfo{key: s}}
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
	canceled := make(chan struct{})
	runDone := make(chan struct{})
	s := &streamInfo{
		cancel:  func() { close(canceled) },
		runDone: runDone,
		running: true,
	}
	m := &manager{
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
		m.active[differentKey] = &streamInfo{}
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
		paths.subtitlePlaylist,
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
