package torrent

import (
	"context"
	"errors"
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

func TestStreamInfoWaitReadyReturnsSignalError(t *testing.T) {
	want := errors.New("probe failed")
	s := &streamInfo{}
	runID := s.beginRun()
	s.signalReady(runID, want)

	if got := s.waitReady(context.Background()); !errors.Is(got, want) {
		t.Fatalf("waitReady() = %v, want %v", got, want)
	}
}

func TestStreamInfoWaitReadyHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()

	s := &streamInfo{}
	s.beginRun()

	if err := s.waitReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitReady() = %v, want deadline exceeded", err)
	}
}

func TestStreamInfoIgnoresStaleRunReadySignal(t *testing.T) {
	s := &streamInfo{}
	staleRunID := s.beginRun()
	currentRunID := s.beginRun()

	if ok := s.signalReady(staleRunID, errors.New("stale failure")); ok {
		t.Fatal("signalReady() accepted stale run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if err := s.waitReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitReady() = %v, want current run to remain pending", err)
	}

	if ok := s.signalReady(currentRunID, nil); !ok {
		t.Fatal("signalReady() rejected current run")
	}
	if err := s.waitReady(context.Background()); err != nil {
		t.Fatalf("waitReady() = %v, want nil", err)
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

	m.startConversionLocked(key, s)
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("startConversionLocked() removed completed HLS cache: %v", err)
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
