package torrent

import (
	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
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

	status := stream.playbackStatus(-1, playbackTimeline{}, time.Now(), 0, 0, true)
	if status.Generation != "generation-2" {
		t.Fatalf("generation = %q, want generation-2", status.Generation)
	}
}

func TestPlaybackStatusWaitsForSelectedSubtitleTarget(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	stream := &streamInfo{
		ready: ready,
		mediaInfo: domain.MediaInfo{
			Duration: 120,
			Seekable: true,
		},
		materializedWindows: map[int][]ffmpeg.HLSFragment{
			0: {{Start: 0, Duration: 2, Name: videoSegmentName(0)}},
		},
		publishedFragments: []ffmpeg.HLSFragment{
			{Start: 0, Duration: 2, Name: videoSegmentName(0)},
		},
		status:       domain.HlsStatus{Phase: domain.HlsPhasePreparing},
		videoJobs:    make(map[*segmentJob]struct{}),
		subtitleJobs: make(map[*segmentJob]struct{}),
	}
	timeline := newPlaybackTimeline(2*time.Second, 15, 120)

	waiting := stream.playbackStatus(0, timeline, time.Now(), 1, 1, false)
	if waiting.Phase != domain.HlsPhasePreparing || waiting.Message != "Preparing selected subtitles" {
		t.Fatalf("status before subtitle = %+v", waiting)
	}
	readyStatus := stream.playbackStatus(0, timeline, time.Now(), 1, 1, true)
	if readyStatus.Phase != domain.HlsPhaseReady {
		t.Fatalf("status after subtitle = %+v", readyStatus)
	}
}

func TestPlaybackStatusUsesOnlyThePublishedPresentationForReadiness(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	now := time.Now()
	job := &segmentJob{
		begin:        0,
		end:          1,
		stage:        domain.HlsStagePackaging,
		startedAt:    now,
		lastProgress: now,
	}
	stream := &streamInfo{
		ready: ready,
		mediaInfo: domain.MediaInfo{
			Duration: 120,
			Seekable: true,
		},
		materializedWindows: map[int][]ffmpeg.HLSFragment{
			0: {{Start: 0, Duration: 2, Name: videoSegmentName(0)}},
		},
		status:    domain.HlsStatus{Phase: domain.HlsPhasePreparing},
		videoJobs: map[*segmentJob]struct{}{job: {}},
	}
	timeline := newPlaybackTimeline(2*time.Second, 15, 120)

	changing := stream.playbackStatus(0, timeline, now, 1, 1, true)
	if changing.Phase != domain.HlsPhasePreparing {
		t.Fatalf("status during foreground presentation change = %+v", changing)
	}

	stream.publishedFragments = append([]ffmpeg.HLSFragment(nil), stream.materializedWindows[0]...)
	published := stream.playbackStatus(0, timeline, now, 1, 1, true)
	if published.Phase != domain.HlsPhaseReady {
		t.Fatalf("status after target publication = %+v", published)
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

	stream.recordSourceBytes(background.id, 0, 10)
	stream.recordSourceBytes(subtitle.id, 0, 20)
	if stream.status.BytesRead != 0 || !stream.status.LastProgress.Equal(started) {
		t.Fatalf("unrelated work changed preparation status: %+v", stream.status)
	}
	stream.recordSourceBytes(requested.id, 0, 30)
	if stream.status.BytesRead != 30 || !stream.status.LastProgress.After(started) {
		t.Fatalf("requested video work did not advance preparation status: %+v", stream.status)
	}
	if background.bytesRead != 10 || subtitle.bytesRead != 20 || requested.bytesRead != 30 {
		t.Fatalf("per-job byte accounting was lost: background=%d subtitle=%d requested=%d", background.bytesRead, subtitle.bytesRead, requested.bytesRead)
	}
}

func TestAddSourceRangeCountsOnlyUniqueBytes(t *testing.T) {
	ranges, unique := addSourceRange(nil, 100, 200)
	if unique != 100 {
		t.Fatalf("first unique bytes = %d, want 100", unique)
	}
	ranges, unique = addSourceRange(ranges, 125, 175)
	if unique != 0 {
		t.Fatalf("repeated unique bytes = %d, want 0", unique)
	}
	ranges, unique = addSourceRange(ranges, 150, 250)
	if unique != 50 {
		t.Fatalf("overlap unique bytes = %d, want 50", unique)
	}
	ranges, unique = addSourceRange(ranges, 50, 110)
	if unique != 50 {
		t.Fatalf("prefix unique bytes = %d, want 50", unique)
	}
	if len(ranges) != 1 || ranges[0] != (sourceByteRange{start: 50, end: 250}) {
		t.Fatalf("merged ranges = %+v, want [{50 250}]", ranges)
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
	scheduler := newSegmentScheduler(2, 1, 1)

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

func TestSessionAdmissionExplicitTargetCancelsSupersededUnobservedTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &streamInfo{
		ctx:          ctx,
		videoJobs:    make(map[*segmentJob]struct{}),
		subtitleJobs: make(map[*segmentJob]struct{}),
	}
	scheduler := newSegmentScheduler(2, 1, 1)

	stream.mtx.Lock()
	first, firstCtx, _, err := stream.acquireJobLocked(videoSegmentJob, 0, 0, 5, false, scheduler, 2)
	if err != nil {
		stream.mtx.Unlock()
		t.Fatal(err)
	}
	claimVideoTargetLocked(stream.videoJobs, 10, 15, 20)
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
	scheduler := newSegmentScheduler(2, 1, 1)

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

func TestSessionAdmissionRequiredSubtitleReplacesBackgroundSubtitle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &streamInfo{
		ctx:          ctx,
		videoJobs:    make(map[*segmentJob]struct{}),
		subtitleJobs: make(map[*segmentJob]struct{}),
	}
	scheduler := newSegmentScheduler(2, 1, 1)

	stream.mtx.Lock()
	background, backgroundCtx, _, err := stream.acquireJobLocked(subtitleSegmentJob, 12, 12, 13, true, scheduler, 2)
	if err != nil {
		stream.mtx.Unlock()
		t.Fatal(err)
	}
	required, _, created, err := stream.acquireJobLocked(subtitleSegmentJob, 12, 12, 13, false, scheduler, 2)
	stream.mtx.Unlock()
	if err != nil || !created || required == background || required.background {
		t.Fatalf("required subtitle admission = %p, %v, created=%t; background=%p", required, err, created, background)
	}
	select {
	case <-backgroundCtx.Done():
	default:
		t.Fatal("required subtitle did not cancel its background predecessor")
	}
	background.releaseAdmission()
	required.releaseAdmission()
}

func TestSessionAdmissionForegroundPreemptsFullBackgroundBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &streamInfo{
		ctx:          ctx,
		videoJobs:    make(map[*segmentJob]struct{}),
		subtitleJobs: make(map[*segmentJob]struct{}),
	}
	scheduler := newSegmentScheduler(3, 1, 1)

	stream.mtx.Lock()
	observed, _, _, err := stream.acquireJobLocked(videoSegmentJob, 0, 0, 1, false, scheduler, 3)
	if err != nil {
		stream.mtx.Unlock()
		t.Fatal(err)
	}
	observed.waiters = 1
	_, firstSubtitleCtx, _, err := stream.acquireJobLocked(subtitleSegmentJob, 0, 0, 1, true, scheduler, 3)
	if err != nil {
		stream.mtx.Unlock()
		t.Fatal(err)
	}
	_, secondSubtitleCtx, _, err := stream.acquireJobLocked(subtitleSegmentJob, 1, 1, 2, true, scheduler, 3)
	if err != nil {
		stream.mtx.Unlock()
		t.Fatal(err)
	}
	foreground, _, created, err := stream.acquireJobLocked(videoSegmentJob, 10, 10, 11, false, scheduler, 3)
	stream.mtx.Unlock()
	if err != nil || !created {
		t.Fatalf("foreground admission = %v, created=%t", err, created)
	}
	select {
	case <-firstSubtitleCtx.Done():
	case <-secondSubtitleCtx.Done():
	default:
		t.Fatal("foreground work did not preempt the background subtitle backlog")
	}
	if len(stream.videoJobs)+len(stream.subtitleJobs) != 3 || len(scheduler.jobs) != 3 {
		t.Fatalf("jobs after preemption = session:%d scheduler:%d, want 3/3", len(stream.videoJobs)+len(stream.subtitleJobs), len(scheduler.jobs))
	}
	observed.releaseAdmission()
	foreground.releaseAdmission()
}

func TestExplicitTargetRetiresObservedPreviousWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &streamInfo{
		ctx:          ctx,
		videoJobs:    make(map[*segmentJob]struct{}),
		subtitleJobs: make(map[*segmentJob]struct{}),
	}
	scheduler := newSegmentScheduler(1, 1, 1)

	stream.mtx.Lock()
	previous, previousCtx, _, err := stream.acquireJobLocked(videoSegmentJob, 0, 0, 1, false, scheduler, 1)
	if err != nil {
		stream.mtx.Unlock()
		t.Fatal(err)
	}
	previous.waiters = 1
	claimVideoTargetLocked(stream.videoJobs, 100, 101, 200)
	next, _, created, err := stream.acquireJobLocked(videoSegmentJob, 100, 100, 101, false, scheduler, 1)
	stream.mtx.Unlock()
	if err != nil || !created {
		t.Fatalf("new target admission = %v, created=%t", err, created)
	}
	select {
	case <-previousCtx.Done():
	default:
		t.Fatal("previous observed target was not canceled")
	}
	if _, exists := stream.videoJobs[previous]; exists {
		t.Fatal("previous target still owns the stream admission")
	}
	if len(scheduler.jobs) != 1 {
		t.Fatalf("scheduler jobs = %d, want only the new target", len(scheduler.jobs))
	}
	previous.releaseAdmission()
	next.releaseAdmission()
}

func TestFragmentDemandCannotSupersedeExplicitTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &streamInfo{
		ctx:          ctx,
		videoJobs:    make(map[*segmentJob]struct{}),
		subtitleJobs: make(map[*segmentJob]struct{}),
	}
	scheduler := newSegmentScheduler(1, 1, 1)

	stream.mtx.Lock()
	target, targetCtx, _, err := stream.acquireJobLocked(videoSegmentJob, 100, 100, 101, false, scheduler, 1)
	if err != nil {
		stream.mtx.Unlock()
		t.Fatal(err)
	}
	_, _, _, fragmentErr := stream.acquireJobLocked(videoSegmentJob, 0, 0, 1, false, scheduler, 1)
	stream.mtx.Unlock()
	if !errors.Is(fragmentErr, errStreamJobLimit) {
		t.Fatalf("stale fragment admission = %v, want stream limit", fragmentErr)
	}
	select {
	case <-targetCtx.Done():
		t.Fatal("fragment demand canceled the explicit playback target")
	default:
	}
	if _, active := stream.videoJobs[target]; !active {
		t.Fatal("explicit playback target lost presentation ownership")
	}
	target.releaseAdmission()
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
