package torrent

import (
	"cinemator/domain"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStreamKeyDirNameAndParse(t *testing.T) {
	key := streamKey{InfoHash: "abc123", Index: 7, Audio: 1, Subtitle: -1}
	if got := key.dirName(); got != "abc123_7_a1_s-1_t0" {
		t.Fatalf("dirName() = %q", got)
	}
	parsed, err := parseStreamDir(key.dirName())
	if err != nil || parsed != key {
		t.Fatalf("parseStreamDir() = %#v, %v; want %#v", parsed, err, key)
	}
}

func TestTranscodedStreamKeyHasDistinctStableIdentity(t *testing.T) {
	key := streamKey{InfoHash: "abc123", Index: 7, Audio: 1, Subtitle: -1, Transcode: true}
	if got := key.dirName(); got != "abc123_7_a1_s-1_t1" {
		t.Fatalf("dirName() = %q", got)
	}
	parsed, err := parseStreamDir(key.dirName())
	if err != nil || parsed != key {
		t.Fatalf("parseStreamDir() = %#v, %v; want %#v", parsed, err, key)
	}
	if got := key.paths("/cache").outDir; got != filepath.Join("/cache", key.dirName()) {
		t.Fatalf("transcoded output path = %q", got)
	}
}

func TestPlaybackStatusPublishesCurrentAssetGeneration(t *testing.T) {
	stream := &streamInfo{
		assetVersion: "generation-2",
		status:       domain.HlsStatus{Phase: domain.HlsPhaseWaiting},
	}

	status := stream.playbackStatus(-1, playbackTimeline{}, time.Now(), 0, 0, 0)
	if status.Generation != "generation-2" {
		t.Fatalf("generation = %q, want generation-2", status.Generation)
	}
}

func TestRecordSourceBytesUpdatesOnlyRequestedVideoPreparation(t *testing.T) {
	requested := &segmentJob{begin: 10, end: 15, id: "requested"}
	background := &segmentJob{begin: 0, end: 5, id: "background", background: true}
	subtitle := &segmentJob{begin: 10, end: 15, id: "subtitle"}
	started := time.Now().Add(-time.Minute)
	stream := &streamInfo{
		status:        domain.HlsStatus{Phase: "preparing", LastProgress: started},
		statusSegment: 10,
		videoJobs: map[*segmentJob]struct{}{
			requested:  {},
			background: {},
		},
		subtitleJobs: map[*segmentJob]struct{}{subtitle: {}},
	}

	stream.recordSourceBytes(background.id, 10)
	stream.recordSourceBytes(subtitle.id, 20)
	if stream.status.BytesRead != 0 || !stream.status.LastProgress.Equal(started) {
		t.Fatalf("unrelated work changed preparation status: %+v", stream.status)
	}
	stream.recordSourceBytes(requested.id, 30)
	if stream.status.BytesRead != 30 || !stream.status.LastProgress.After(started) {
		t.Fatalf("requested video work did not advance preparation status: %+v", stream.status)
	}
	if background.bytesRead != 10 || subtitle.bytesRead != 20 || requested.bytesRead != 30 {
		t.Fatalf("per-job byte accounting was lost: background=%d subtitle=%d requested=%d", background.bytesRead, subtitle.bytesRead, requested.bytesRead)
	}
}

func TestSessionAdmissionDeduplicatesOneTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &streamInfo{
		ctx:          ctx,
		videoJobs:    make(map[*segmentJob]struct{}),
		subtitleJobs: make(map[*segmentJob]struct{}),
	}
	scheduler := newSegmentScheduler(2, 1)

	stream.mtx.Lock()
	first, _, created, err := stream.acquireJobLocked(videoSegmentJob, 12, 10, 15, false, scheduler, 2)
	if err != nil || !created {
		t.Fatalf("first admission = %v, created=%t", err, created)
	}
	second, _, created, err := stream.acquireJobLocked(videoSegmentJob, 12, 10, 15, false, scheduler, 2)
	stream.mtx.Unlock()
	if err != nil || created || second != first {
		t.Fatalf("second admission = %p, %v, created=%t; want existing %p", second, err, created, first)
	}
	if len(stream.videoJobs) != 1 || len(scheduler.jobs) != 1 {
		t.Fatalf("jobs = session:%d scheduler:%d, want 1/1", len(stream.videoJobs), len(scheduler.jobs))
	}
	first.releaseAdmission()
}

func TestSessionAdmissionCancelsSupersededUnobservedTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &streamInfo{
		ctx:          ctx,
		videoJobs:    make(map[*segmentJob]struct{}),
		subtitleJobs: make(map[*segmentJob]struct{}),
	}
	scheduler := newSegmentScheduler(2, 1)

	stream.mtx.Lock()
	first, firstCtx, _, err := stream.acquireJobLocked(videoSegmentJob, 0, 0, 5, false, scheduler, 2)
	if err != nil {
		stream.mtx.Unlock()
		t.Fatal(err)
	}
	second, _, _, err := stream.acquireJobLocked(videoSegmentJob, 10, 10, 15, false, scheduler, 2)
	stream.mtx.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstCtx.Done():
	default:
		t.Fatal("superseded unobserved target was not canceled")
	}
	first.releaseAdmission()
	second.releaseAdmission()
}

func TestSessionAdmissionReplacesOverlappingBackgroundWorkForAViewer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &streamInfo{
		ctx:          ctx,
		videoJobs:    make(map[*segmentJob]struct{}),
		subtitleJobs: make(map[*segmentJob]struct{}),
	}
	scheduler := newSegmentScheduler(2, 1)

	stream.mtx.Lock()
	background, backgroundCtx, _, err := stream.acquireJobLocked(videoSegmentJob, 10, 10, 25, true, scheduler, 2)
	if err != nil {
		stream.mtx.Unlock()
		t.Fatal(err)
	}
	foreground, _, created, err := stream.acquireJobLocked(videoSegmentJob, 12, 12, 13, false, scheduler, 2)
	stream.mtx.Unlock()
	if err != nil || !created || foreground == background {
		t.Fatalf("foreground admission = %p, %v, created=%t; background=%p", foreground, err, created, background)
	}
	select {
	case <-backgroundCtx.Done():
	default:
		t.Fatal("overlapping background work was not canceled")
	}
	background.releaseAdmission()
	foreground.releaseAdmission()
}

func TestStreamKeyPaths(t *testing.T) {
	root := t.TempDir()
	paths := (streamKey{InfoHash: "hash", Index: 2, Audio: 0, Subtitle: 3}).paths(root)
	wantDir := filepath.Join(root, "hash_2_a0_s3_t0")
	if paths.outDir != wantDir ||
		paths.videoPlaylist != filepath.Join(wantDir, "index.m3u8") ||
		paths.subtitlePlaylist != filepath.Join(wantDir, "subs.m3u8") ||
		paths.masterPlaylist != filepath.Join(wantDir, "master.m3u8") {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestParseStreamDirRejectsMalformedNames(t *testing.T) {
	for _, name := range []string{"", "hash_1_a0", "hash_x_a0_s0_t0", "hash_1_x0_s0_t0", "hash_1_a0_x0_t0", "hash_1_a0_sx_t0", "hash_1_a0_s0_tx", "hash_1_a0_s0_t0_p12000", "hash_1_a0_s0_t0_g1"} {
		if _, err := parseStreamDir(name); err == nil {
			t.Fatalf("parseStreamDir(%q) succeeded", name)
		}
	}
}

func TestResetStreamOutputRemovesStaleHLSFiles(t *testing.T) {
	paths := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}.paths(t.TempDir())
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(paths.outDir, "chunk_000001.ts")
	if err := os.WriteFile(stale, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	assets, err := newHlsAssetStore(filepath.Dir(paths.outDir))
	if err != nil {
		t.Fatal(err)
	}
	m := manager{media: &mediaCache{assets: assets}}
	if err := m.resetStreamOutput(paths); err != nil {
		t.Fatalf("resetStreamOutput() error = %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale file still exists: %v", err)
	}
}
