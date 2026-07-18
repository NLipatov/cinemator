package torrent

import (
	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
	"cinemator/presentation/settings"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStreamStatusTracksRequestedSegmentProgress(t *testing.T) {
	stream := &streamInfo{
		mediaInfo:     domain.MediaInfo{Duration: 120, Seekable: true},
		statusSegment: -1,
	}
	stream.markPreparing(7, 6*time.Second)
	stream.recordSourceBytes("", 8192)

	stream.mtx.Lock()
	status := stream.status
	stream.mtx.Unlock()
	if status.Phase != "preparing" || status.TargetSeconds != 42 || status.BytesRead != 8192 {
		t.Fatalf("status = %+v", status)
	}

	stream.markReady(7)
	stream.mtx.Lock()
	status = stream.status
	stream.mtx.Unlock()
	if status.Phase != "ready" {
		t.Fatalf("phase = %q, want ready", status.Phase)
	}
}

func TestParseSegmentName(t *testing.T) {
	index, ok := parseSegmentName("chunk_000123.ts", "chunk_", ".ts")
	if !ok || index != 123 {
		t.Fatalf("parseSegmentName() = %d, %t; want 123, true", index, ok)
	}
	for _, name := range []string{
		"chunk_123.ts",
		"chunk_000123.m4s",
		"../chunk_000123.ts",
		"chunk_-00123.ts",
	} {
		if _, ok := parseSegmentName(name, "chunk_", ".ts"); ok {
			t.Fatalf("parseSegmentName(%q) accepted malformed asset", name)
		}
	}
}

func TestWaitForGeneratedAssetReturnsAsSoonAsFileAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chunk.ts")
	job := &segmentJob{done: make(chan struct{})}
	go func() {
		time.Sleep(25 * time.Millisecond)
		_ = os.WriteFile(path, []byte("segment"), 0644)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForGeneratedAsset(ctx, path, job); err != nil {
		t.Fatalf("waitForGeneratedAsset() error = %v", err)
	}
}

func TestWaitForGeneratedAssetReturnsJobFailure(t *testing.T) {
	want := errors.New("ffmpeg failed")
	job := &segmentJob{done: make(chan struct{}), err: want}
	close(job.done)

	err := waitForGeneratedAsset(context.Background(), filepath.Join(t.TempDir(), "missing.ts"), job)
	if !errors.Is(err, want) {
		t.Fatalf("waitForGeneratedAsset() error = %v, want %v", err, want)
	}
}

func TestAdvanceProgressivePlaylistPublishesOnlyGeneratedSegments(t *testing.T) {
	dir := t.TempDir()
	stream := &streamInfo{
		mediaInfo: domain.MediaInfo{},
		paths: streamPaths{
			videoPlaylist: filepath.Join(dir, "index.m3u8"),
		},
	}
	manager := &manager{}
	first := &segmentJob{begin: 0, end: 5, result: ffmpeg.VideoWindowResult{Generated: 5}}
	if err := manager.advanceProgressivePlaylist(stream, first); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stream.paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	playlist := string(data)
	if !strings.Contains(playlist, "chunk_000004.ts") || strings.Contains(playlist, "chunk_000005.ts") {
		t.Fatalf("unexpected progressive playlist:\n%s", playlist)
	}

	end := &segmentJob{begin: 5, end: 10, result: ffmpeg.VideoWindowResult{ReachedEnd: true}}
	if err := manager.advanceProgressivePlaylist(stream, end); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(stream.paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	playlist = string(data)
	if !strings.Contains(playlist, "#EXT-X-ENDLIST") || strings.Contains(playlist, "chunk_000005.ts") {
		t.Fatalf("unexpected final progressive playlist:\n%s", playlist)
	}
}

func TestIndependentSegmentJobsDoNotCancelEachOther(t *testing.T) {
	firstCanceled := false
	secondCanceled := false
	first := &segmentJob{begin: 0, end: 5, done: make(chan struct{}), waiters: 1, cancel: func() { firstCanceled = true }}
	second := &segmentJob{begin: 20, end: 25, done: make(chan struct{}), waiters: 1, cancel: func() { secondCanceled = true }}
	stream := &streamInfo{videoJobs: map[*segmentJob]struct{}{first: {}, second: {}}}
	manager := &manager{}

	manager.releaseJobWaiter(stream, first, context.Canceled)

	if !firstCanceled || secondCanceled {
		t.Fatalf("cancellation leaked between jobs: first=%t second=%t", firstCanceled, secondCanceled)
	}
	if got := findSegmentJob(stream.videoJobs, 22); got != second {
		t.Fatalf("findSegmentJob() = %p, want second job %p", got, second)
	}
}

func TestEnsureHlsAssetRejectsStaleGeneration(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	ready := make(chan struct{})
	close(ready)
	stream := &streamInfo{ready: ready, assetVersion: "current"}
	manager := &manager{active: map[streamKey]*streamInfo{key: stream}}
	if err := manager.ensureHlsAsset(context.Background(), key.dirName(), "index.m3u8", "stale"); !errors.Is(err, domain.ErrHlsPlaylistChanged) {
		t.Fatalf("stale generation error = %v", err)
	}
	if err := manager.ensureHlsAsset(context.Background(), key.dirName(), "master.m3u8", ""); err != nil {
		t.Fatalf("master playlist without version error = %v", err)
	}
}

func TestOpenHlsPlaylistBlocksReplacementUntilClose(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	paths := key.paths(root)
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.videoPlaylist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	stream := &streamInfo{ready: ready, assetVersion: "current", paths: paths}
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := &manager{active: map[streamKey]*streamInfo{key: stream}, assets: assets, settings: settings.NewSettings()}
	asset, err := manager.OpenHlsAsset(context.Background(), key.dirName(), "index.m3u8", "current")
	if err != nil {
		t.Fatal(err)
	}
	attempted := make(chan struct{})
	locked := make(chan struct{})
	go func() {
		close(attempted)
		stream.playlistMtx.Lock()
		close(locked)
		stream.playlistMtx.Unlock()
	}()
	<-attempted
	select {
	case <-locked:
		t.Fatal("playlist writer passed an active response lease")
	case <-time.After(20 * time.Millisecond):
	}
	if err := asset.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("playlist writer did not resume after response close")
	}
}

func TestStartedSegmentJobSurvivesDisconnectedWaiter(t *testing.T) {
	canceled := false
	job := &segmentJob{begin: 0, end: 5, done: make(chan struct{}), waiters: 1, started: true, cancel: func() { canceled = true }}
	stream := &streamInfo{videoJobs: map[*segmentJob]struct{}{job: {}}}
	(&manager{}).releaseJobWaiter(stream, job, context.Canceled)
	if canceled {
		t.Fatal("started bounded job was canceled after its last waiter disconnected")
	}
}

func TestNewWindowCancelsAbandonedStartedJob(t *testing.T) {
	canceled := false
	job := &segmentJob{begin: 0, end: 5, done: make(chan struct{}), started: true, cancel: func() { canceled = true }}
	cancelAbandonedJobsLocked(map[*segmentJob]struct{}{job: {}}, 10, 15)
	if !canceled {
		t.Fatal("abandoned old window was not canceled for a newer seek")
	}
}

func TestNewWindowKeepsOverlappingOrObservedJob(t *testing.T) {
	for _, job := range []*segmentJob{
		{begin: 10, end: 15, done: make(chan struct{}), cancel: func() { t.Error("overlapping job was canceled") }},
		{begin: 0, end: 5, done: make(chan struct{}), waiters: 1, cancel: func() { t.Error("observed job was canceled") }},
	} {
		cancelAbandonedJobsLocked(map[*segmentJob]struct{}{job: {}}, 10, 15)
	}
}

func TestSourceProgressIsAttributedToMatchingJob(t *testing.T) {
	video := &segmentJob{id: "video", done: make(chan struct{})}
	subtitle := &segmentJob{id: "subtitle", done: make(chan struct{})}
	stream := &streamInfo{
		status:       domain.HlsStatus{Phase: "preparing"},
		videoJobs:    map[*segmentJob]struct{}{video: {}},
		subtitleJobs: map[*segmentJob]struct{}{subtitle: {}},
	}
	stream.recordSourceBytes("video", 4096)
	if video.bytesRead != 4096 || subtitle.bytesRead != 0 {
		t.Fatalf("job bytes: video=%d subtitle=%d", video.bytesRead, subtitle.bytesRead)
	}
	stream.recordSourceBytes("subtitle", 1024)
	if video.bytesRead != 4096 || subtitle.bytesRead != 1024 {
		t.Fatalf("job bytes after subtitle read: video=%d subtitle=%d", video.bytesRead, subtitle.bytesRead)
	}
}

func TestSegmentWindowCreatesCanonicalNonOverlappingRanges(t *testing.T) {
	for _, index := range []int{10, 11, 12, 13, 14} {
		begin, end := segmentWindow(index, 100, 5)
		if begin != 10 || end != 15 {
			t.Fatalf("segmentWindow(%d) = [%d,%d), want [10,15)", index, begin, end)
		}
	}
	begin, end := segmentWindow(99, 100, 5)
	if begin != 95 || end != 100 {
		t.Fatalf("tail window = [%d,%d), want [95,100)", begin, end)
	}
}

func TestParseDirectSegmentOwner(t *testing.T) {
	for _, name := range []string{"direct_000015_0003.ts", "direct_000015_0003.m4s"} {
		owner, ok := parseDirectSegmentOwner(name)
		if !ok || owner != 15 {
			t.Fatalf("parseDirectSegmentOwner(%q) = %d, %v", name, owner, ok)
		}
	}
	for _, name := range []string{"direct_15_0003.ts", "direct_000015.ts", "direct_000015_0003.mp4", "../direct_000015_0003.ts"} {
		if _, ok := parseDirectSegmentOwner(name); ok {
			t.Fatalf("parseDirectSegmentOwner(%q) accepted invalid name", name)
		}
	}
	if owner, ok := parseDirectInitOwner("init_000015.mp4"); !ok || owner != 15 {
		t.Fatalf("parseDirectInitOwner() = %d, %v", owner, ok)
	}
	for _, name := range []string{"seek_000015.ts", "seek_000015.m4s", "init_seek_000015.mp4"} {
		if index, ok := parseSeekAsset(name); !ok || index != 15 {
			t.Fatalf("parseSeekAsset(%q) = %d, %v", name, index, ok)
		}
	}
}

func TestReserveJobSlotEnforcesGlobalAndPerStreamLimits(t *testing.T) {
	manager := &manager{jobs: make(chan struct{}, 1)}
	stream := &streamInfo{videoJobs: make(map[*segmentJob]struct{}), subtitleJobs: make(map[*segmentJob]struct{})}
	if err := manager.reserveJobSlotLocked(stream); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if err := manager.reserveJobSlotLocked(stream); !errors.Is(err, errStreamJobQueueFull) {
		t.Fatalf("second reservation = %v, want queue full", err)
	}
	<-manager.jobs
	for index := 0; index < manager.settings.MaxJobsPerStream(); index++ {
		stream.videoJobs[&segmentJob{}] = struct{}{}
	}
	if err := manager.reserveJobSlotLocked(stream); !errors.Is(err, errStreamJobLimit) {
		t.Fatalf("per-stream reservation = %v, want stream limit", err)
	}
}

func TestSubtitleJobFailureBecomesVisible(t *testing.T) {
	job := &segmentJob{begin: 0, end: 1, done: make(chan struct{})}
	stream := &streamInfo{
		status:       domain.HlsStatus{Phase: "preparing"},
		subtitleJobs: map[*segmentJob]struct{}{job: {}},
	}
	stream.markJobError(job, errors.New("ffmpeg failed"))

	if stream.status.Phase != "error" || !strings.Contains(stream.status.Message, "FFmpeg") {
		t.Fatalf("status = %+v", stream.status)
	}
}

func TestImmediateQueueFailureBecomesVisible(t *testing.T) {
	stream := &streamInfo{
		status:        domain.HlsStatus{Phase: "preparing"},
		statusSegment: 7,
	}
	stream.markSegmentError(7, errStreamJobQueueFull)
	if stream.status.Phase != "error" || !strings.Contains(stream.status.Message, "capacity") {
		t.Fatalf("status = %+v", stream.status)
	}
}

func TestClassifyHlsStatusDistinguishesNoPeersAndStalledWork(t *testing.T) {
	now := time.Now()
	base := domain.HlsStatus{Phase: "preparing", LastProgress: now.Add(-20 * time.Second)}
	noPeers := classifyHlsStatus(base, now)
	if noPeers.Phase != "no_peers" {
		t.Fatalf("no-peer phase = %q", noPeers.Phase)
	}
	base.ActivePeers = 1
	stalled := classifyHlsStatus(base, now)
	if stalled.Phase != "stalled" {
		t.Fatalf("stalled phase = %q", stalled.Phase)
	}
	base.LastProgress = now
	if active := classifyHlsStatus(base, now); active.Phase != "preparing" {
		t.Fatalf("active phase = %q", active.Phase)
	}
}

func TestPublishProgressiveSegmentAdvertisesOnlyCompletedAssets(t *testing.T) {
	dir := t.TempDir()
	stream := &streamInfo{
		paths: streamPaths{videoPlaylist: filepath.Join(dir, "index.m3u8")},
	}
	manager := &manager{}
	if err := manager.publishProgressiveSegment(stream, 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stream.paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "chunk_000000.ts") || strings.Contains(string(data), "chunk_000001.ts") {
		t.Fatalf("unexpected playlist:\n%s", data)
	}
}

func TestReconcileKnownDurationShrinksOverstatedTimeline(t *testing.T) {
	dir := t.TempDir()
	stream := &streamInfo{
		mediaInfo: domain.MediaInfo{Duration: 120, Seekable: true},
		status:    domain.HlsStatus{Duration: 120, Seekable: true},
		paths:     streamPaths{videoPlaylist: filepath.Join(dir, "index.m3u8")},
	}
	job := &segmentJob{
		begin:  5,
		result: ffmpeg.VideoWindowResult{Generated: 2, Durations: []float64{6, 2}, ReachedEnd: true},
	}
	manager := &manager{}
	if err := manager.reconcileKnownDuration(stream, job); err != nil {
		t.Fatal(err)
	}
	if stream.mediaInfo.Duration != 38 || stream.status.Duration != 38 {
		t.Fatalf("durations = media %.1f status %.1f", stream.mediaInfo.Duration, stream.status.Duration)
	}
	data, err := os.ReadFile(stream.paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "chunk_000006.ts") || strings.Contains(string(data), "chunk_000007.ts") {
		t.Fatalf("unexpected reconciled playlist:\n%s", data)
	}
}
