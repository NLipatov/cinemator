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
	"sync"
	"testing"
	"time"
)

func TestStreamStatusTracksRequestedSegmentProgress(t *testing.T) {
	stream := &streamInfo{
		mediaInfo:     domain.MediaInfo{Duration: 120, Seekable: true},
		statusSegment: -1,
		videoJobs:     make(map[*segmentJob]struct{}),
	}
	stream.markPreparing(7, 42)
	job := &segmentJob{begin: 5, end: 10, id: "requested"}
	stream.videoJobs[job] = struct{}{}
	stream.recordSourceBytes(job.id, 0, 8192)

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

func TestPackagerWaitsInPlaceForMissingTorrentData(t *testing.T) {
	now := time.Now()
	stage, err := classifyPackagerObservation(packagerObservation{
		now:                now,
		sourceProgress:     now.Add(-time.Second),
		outputProgress:     now.Add(-time.Second),
		diagnosticProgress: now.Add(-time.Second),
		rangeStart:         8 << 20,
		rangeEnd:           16 << 20,
		missingPieces:      1,
		noOutputDeadline:   10 * time.Second,
	})

	if err != nil {
		t.Fatalf("missing torrent data stopped the active media process: %v", err)
	}
	if stage != domain.HlsStageSourceBlocked {
		t.Fatalf("stage = %q, want %q", stage, domain.HlsStageSourceBlocked)
	}
}

func TestPackagerFailsWhenCompleteSourceMakesNoProgress(t *testing.T) {
	now := time.Now()
	stage, err := classifyPackagerObservation(packagerObservation{
		now:                now,
		sourceProgress:     now.Add(-time.Minute),
		outputProgress:     now.Add(-time.Minute),
		diagnosticProgress: now.Add(-time.Minute),
		rangeStart:         8 << 20,
		rangeEnd:           16 << 20,
		noOutputDeadline:   10 * time.Second,
	})

	if !errors.Is(err, errPackagerNoOutput) {
		t.Fatalf("complete source stall = %v, want %v", err, errPackagerNoOutput)
	}
	if stage != domain.HlsStageError {
		t.Fatalf("stage = %q, want %q", stage, domain.HlsStageError)
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
	dir := t.TempDir()
	assets, err := newHlsAssetStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer assets.Close()
	path := filepath.Join(dir, "chunk.ts")
	job := &segmentJob{done: make(chan struct{}), progress: make(chan struct{}, 1)}
	go func() {
		time.Sleep(25 * time.Millisecond)
		_ = os.WriteFile(path, []byte("segment"), 0644)
		job.notifyProgress()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForGeneratedAsset(ctx, &mediaCache{assets: assets}, path, job); err != nil {
		t.Fatalf("waitForGeneratedAsset() error = %v", err)
	}
}

func TestWaitForGeneratedAssetReturnsJobFailure(t *testing.T) {
	want := errors.New("ffmpeg failed")
	job := &segmentJob{done: make(chan struct{}), err: want}
	close(job.done)

	dir := t.TempDir()
	assets, openErr := newHlsAssetStore(dir)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer assets.Close()
	err := waitForGeneratedAsset(context.Background(), &mediaCache{assets: assets}, filepath.Join(dir, "missing.ts"), job)
	if !errors.Is(err, want) {
		t.Fatalf("waitForGeneratedAsset() error = %v, want %v", err, want)
	}
}

func TestWaitForDirectTargetReturnsBeforeTheWindowJobCompletes(t *testing.T) {
	stream := &streamInfo{materializedWindows: make(map[int][]ffmpeg.HLSFragment)}
	job := &segmentJob{
		targetSeconds: 0.5,
		done:          make(chan struct{}),
		progress:      make(chan struct{}, 1),
	}
	result := make(chan error, 1)
	go func() {
		result <- waitForDirectTarget(context.Background(), stream, job)
	}()

	stream.mtx.Lock()
	stream.materializedWindows[0] = []ffmpeg.HLSFragment{{Start: 0, Duration: 2, Name: videoSegmentName(0)}}
	stream.mtx.Unlock()
	job.notifyProgress()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("waitForDirectTarget() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waitForDirectTarget() waited for the rest of the direct window")
	}
	select {
	case <-job.done:
		t.Fatal("test job completed before startup became ready")
	default:
	}
}

func TestIndependentSegmentJobsDoNotCancelEachOther(t *testing.T) {
	firstCanceled := false
	secondCanceled := false
	first := &segmentJob{begin: 0, end: 5, done: make(chan struct{}), waiters: 1, cancel: func() { firstCanceled = true }}
	second := &segmentJob{begin: 20, end: 25, done: make(chan struct{}), waiters: 1, cancel: func() { secondCanceled = true }}
	stream := &streamInfo{videoJobs: map[*segmentJob]struct{}{first: {}, second: {}}}
	stream.releaseJobWaiter(first, context.Canceled)

	if !firstCanceled || secondCanceled {
		t.Fatalf("cancellation leaked between jobs: first=%t second=%t", firstCanceled, secondCanceled)
	}
	if got := findSegmentJob(stream.videoJobs, 22); got != second {
		t.Fatalf("findSegmentJob() = %p, want second job %p", got, second)
	}
}

func TestEnsureHlsAssetRejectsStaleGeneration(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	root := t.TempDir()
	paths := key.paths(root)
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	direct := "direct_000000_0000.ts"
	if err := os.WriteFile(filepath.Join(paths.outDir, direct), []byte("immutable"), 0644); err != nil {
		t.Fatal(err)
	}
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	stream := &streamInfo{ready: ready, assetVersion: "current", paths: paths}
	manager := &manager{active: map[streamKey]*streamInfo{key: stream}, media: &mediaCache{assets: assets}}
	if err := manager.ensureHlsAsset(context.Background(), key.dirName(), "index.m3u8", "stale"); !errors.Is(err, domain.ErrHlsPlaylistChanged) {
		t.Fatalf("stale generation error = %v", err)
	}
	if err := manager.ensureHlsAsset(context.Background(), key.dirName(), direct, "stale"); err != nil {
		t.Fatalf("immutable stale segment error = %v", err)
	}
	if err := manager.ensureHlsAsset(context.Background(), key.dirName(), "direct_000000_0001.ts", "stale"); !errors.Is(err, domain.ErrHlsPlaylistChanged) {
		t.Fatalf("missing stale segment error = %v", err)
	}
	if err := manager.ensureHlsAsset(context.Background(), key.dirName(), "master.m3u8", ""); err != nil {
		t.Fatalf("master playlist without version error = %v", err)
	}
	if err := manager.ensureHlsAsset(context.Background(), key.dirName(), "master.m3u8", "stale"); !errors.Is(err, domain.ErrHlsPlaylistChanged) {
		t.Fatalf("versioned stale master error = %v", err)
	}
}

func TestVideoPlaylistRequestRestartsStartupGeneration(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}
	ready := make(chan struct{})
	close(ready)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &streamInfo{
		ctx:          ctx,
		ready:        ready,
		assetVersion: "current",
		paths:        key.paths(t.TempDir()),
		mediaInfo:    domain.MediaInfo{Duration: 120},
		videoJobs:    make(map[*segmentJob]struct{}),
		subtitleJobs: make(map[*segmentJob]struct{}),
	}
	scheduler := newSegmentScheduler(1, 1, 1)
	_, _ = scheduler.reserveJob(false, func() {})
	manager := &manager{active: map[streamKey]*streamInfo{key: stream}, scheduler: scheduler, settings: settings.NewSettings()}

	if err := manager.ensureHlsAsset(context.Background(), key.dirName(), "index.m3u8", "current"); err != nil {
		t.Fatal(err)
	}
	stream.mtx.Lock()
	retrying := stream.progressiveRetry
	stream.mtx.Unlock()
	if !retrying {
		t.Fatal("video playlist request did not restart progressive generation")
	}

	cancel()
	stream.mtx.Lock()
	stream.progressiveRetry = false
	stream.playlistTargetDuration = 12 * time.Second
	stream.mtx.Unlock()
	if err := manager.ensureHlsAsset(context.Background(), key.dirName(), "index.m3u8", "current"); err != nil {
		t.Fatal(err)
	}
	stream.mtx.Lock()
	retrying = stream.progressiveRetry
	stream.mtx.Unlock()
	if retrying {
		t.Fatal("materialized playlist started generation without segment demand")
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
	stream := &streamInfo{ctx: context.Background(), ready: ready, assetVersion: "current", paths: paths, sourceEnded: true}
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := &manager{active: map[streamKey]*streamInfo{key: stream}, media: &mediaCache{assets: assets}, settings: settings.NewSettings()}
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

func TestLockedHlsAssetUnlocksPlaylistAfterCloseError(t *testing.T) {
	want := errors.New("close failed")
	var playlist sync.RWMutex
	playlist.RLock()
	asset := &lockedHlsAsset{
		ReadSeekCloser: &failingReadSeekCloser{Reader: strings.NewReader("playlist"), err: want},
		unlock:         playlist.RUnlock,
	}
	if err := asset.Close(); !errors.Is(err, want) {
		t.Fatalf("Close() error = %v, want %v", err, want)
	}
	if err := asset.Close(); !errors.Is(err, want) {
		t.Fatalf("second Close() error = %v, want %v", err, want)
	}
	locked := make(chan struct{})
	go func() {
		playlist.Lock()
		close(locked)
		playlist.Unlock()
	}()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("playlist remained locked after asset close failure")
	}
}

func TestPublishProgressiveSubtitleRetriesAfterPlaylistWriteFailure(t *testing.T) {
	root := t.TempDir()
	playlist := filepath.Join(root, "missing", "subs.m3u8")
	stream := &streamInfo{
		paths:           streamPaths{subtitlePlaylist: playlist},
		assetVersion:    "v1",
		materializedEnd: 1,
	}
	manager := &manager{settings: settings.NewSettings()}
	if err := manager.publishProgressiveSubtitle(stream, 0); err == nil {
		t.Fatal("subtitle publication unexpectedly succeeded without its directory")
	}
	if stream.progressiveSubtitles != 0 {
		t.Fatalf("subtitle progress advanced after failed write: %d", stream.progressiveSubtitles)
	}
	if err := os.Mkdir(filepath.Dir(playlist), 0755); err != nil {
		t.Fatal(err)
	}
	if err := manager.publishProgressiveSubtitle(stream, 0); err != nil {
		t.Fatalf("subtitle retry failed: %v", err)
	}
	if stream.progressiveSubtitles != 1 {
		t.Fatalf("subtitle progress after retry = %d", stream.progressiveSubtitles)
	}
	data, err := os.ReadFile(playlist)
	if err != nil || !strings.Contains(string(data), "subs_000000.vtt?v=v1") {
		t.Fatalf("subtitle playlist = %q, %v", data, err)
	}
}

type failingReadSeekCloser struct {
	*strings.Reader
	err error
}

func (f *failingReadSeekCloser) Close() error { return f.err }

func TestStartedSegmentJobSurvivesDisconnectedWaiter(t *testing.T) {
	canceled := false
	job := &segmentJob{begin: 0, end: 5, done: make(chan struct{}), waiters: 1, started: true, cancel: func() { canceled = true }}
	stream := &streamInfo{videoJobs: map[*segmentJob]struct{}{job: {}}}
	stream.releaseJobWaiter(job, context.Canceled)
	if canceled {
		t.Fatal("started bounded job was canceled after its last waiter disconnected")
	}
}

func TestExplicitTargetCancelsSupersededStartedJob(t *testing.T) {
	canceled := false
	job := &segmentJob{begin: 0, end: 5, done: make(chan struct{}), started: true, cancel: func() { canceled = true }}
	jobs := map[*segmentJob]struct{}{job: {}}
	claimVideoTargetLocked(jobs, 10, 15, 20)
	if !canceled {
		t.Fatal("superseded old window was not canceled for an explicit seek")
	}
	if len(jobs) != 0 {
		t.Fatal("superseded old window retained presentation ownership")
	}
}

func TestExplicitTargetKeepsAndRetargetsOverlappingJob(t *testing.T) {
	job := &segmentJob{
		begin:         10,
		end:           15,
		done:          make(chan struct{}),
		targetSeconds: 20,
		cancel:        func() { t.Error("overlapping job was canceled") },
	}
	jobs := map[*segmentJob]struct{}{job: {}}
	claimVideoTargetLocked(jobs, 12, 13, 24)
	if len(jobs) != 1 || job.targetSeconds != 24 {
		t.Fatalf("overlapping target = jobs:%d target:%.1f, want 1/24", len(jobs), job.targetSeconds)
	}
}

func TestRetiredVideoJobCannotPublishPresentation(t *testing.T) {
	jobCtx, cancel := context.WithCancel(context.Background())
	job := &segmentJob{
		begin:         0,
		end:           1,
		done:          make(chan struct{}),
		ctx:           jobCtx,
		targetSeconds: 0,
	}
	stream := &streamInfo{
		directPlay:          true,
		videoJobs:           make(map[*segmentJob]struct{}),
		materializedWindows: make(map[int][]ffmpeg.HLSFragment),
	}
	cancel()
	manager := &manager{settings: settings.NewSettings()}
	err := manager.publishMaterializedWindow(stream, job, []ffmpeg.HLSFragment{{Start: 0, Duration: 2, Name: "retired.ts"}}, true, true, 1, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("retired publish = %v, want canceled", err)
	}
	if len(stream.materializedWindows) != 0 {
		t.Fatalf("retired job changed presentation: %#v", stream.materializedWindows)
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
	stream.recordSourceBytes("video", 0, 4096)
	if video.bytesRead != 4096 || subtitle.bytesRead != 0 {
		t.Fatalf("job bytes: video=%d subtitle=%d", video.bytesRead, subtitle.bytesRead)
	}
	stream.recordSourceBytes("subtitle", 0, 1024)
	if video.bytesRead != 4096 || subtitle.bytesRead != 1024 {
		t.Fatalf("job bytes after subtitle read: video=%d subtitle=%d", video.bytesRead, subtitle.bytesRead)
	}
}

func TestSegmentWindowCreatesCanonicalNonOverlappingRanges(t *testing.T) {
	timeline := newPlaybackTimeline(6*time.Second, 5, 600)
	for _, index := range []int{10, 11, 12, 13, 14} {
		begin, end := timeline.windowForSegment(index)
		if begin != 10 || end != 15 {
			t.Fatalf("windowForSegment(%d) = [%d,%d), want [10,15)", index, begin, end)
		}
	}
	begin, end := timeline.windowForSegment(99)
	if begin != 95 || end != 100 {
		t.Fatalf("tail window = [%d,%d), want [95,100)", begin, end)
	}
}

func TestProgressivePrefetchPlan(t *testing.T) {
	window := playbackCacheWindow{
		sideBytes:       600,
		bytesPerSegment: 10,
		maximumJob:      15,
		segmentDuration: 2 * time.Second,
		urgentReserve:   30 * time.Second,
	}
	for _, test := range []struct {
		name         string
		demand       int
		materialized int
		total        int
		startup      bool
		want         materializationPlan
	}{
		{
			name:    "startup publishes one segment",
			demand:  100,
			total:   200,
			startup: true,
			want:    materializationPlan{begin: 100, end: 101},
		},
		{
			name:         "foreground fills urgent reserve",
			demand:       0,
			materialized: 1,
			total:        200,
			want:         materializationPlan{begin: 1, end: 15},
		},
		{
			name:         "background continues past time reserve",
			demand:       0,
			materialized: 15,
			total:        200,
			want:         materializationPlan{begin: 15, end: 30, background: true},
		},
		{
			name:         "byte boundary stops prefetch",
			demand:       0,
			materialized: 60,
			total:        200,
			want:         materializationPlan{},
		},
		{
			name:         "tail is capped by duration",
			demand:       95,
			materialized: 99,
			total:        100,
			want:         materializationPlan{begin: 99, end: 100},
		},
		{
			name:         "forward seek refills from requested position",
			demand:       660,
			materialized: 661,
			total:        1000,
			want:         materializationPlan{begin: 661, end: 675},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			timeline := newPlaybackTimeline(2*time.Second, 15, float64(test.total*2))
			windows := make(map[int][]ffmpeg.HLSFragment)
			for index := test.demand; index < test.materialized; index++ {
				windows[index] = []ffmpeg.HLSFragment{{Start: float64(index * 2), Duration: 2}}
			}
			got := window.plan(windows, nil, timeline, test.demand, test.total, test.startup)
			if got != test.want {
				t.Fatalf("playbackCacheWindow.plan() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestStreamDeliveryRiskRaisesAndRecoversTheForwardReserve(t *testing.T) {
	now := time.Now()
	stream := &streamInfo{urgentAhead: 30 * time.Second}
	stream.recordJobDelivery(&segmentJob{begin: 0, end: 5, startedAt: now.Add(-9 * time.Second)}, 2*time.Second, now)
	if stream.urgentAhead != 60*time.Second {
		t.Fatalf("target after marginal delivery = %v, want 60s", stream.urgentAhead)
	}

	for range 12 {
		stream.recordJobDelivery(&segmentJob{begin: 0, end: 5, startedAt: now.Add(-time.Second)}, 2*time.Second, now)
	}
	if stream.urgentAhead != 30*time.Second {
		t.Fatalf("target after sustained fast delivery = %v, want 30s", stream.urgentAhead)
	}
}

func TestProgressivePrefetchPlanUsesAdaptiveReserveOnlyForUrgency(t *testing.T) {
	window := playbackCacheWindow{
		sideBytes:       600,
		bytesPerSegment: 10,
		maximumJob:      15,
		segmentDuration: 2 * time.Second,
		urgentReserve:   60 * time.Second,
	}
	timeline := newPlaybackTimeline(2*time.Second, 15, 200)
	windows := make(map[int][]ffmpeg.HLSFragment)
	for index := 0; index < 15; index++ {
		windows[index] = []ffmpeg.HLSFragment{{Start: float64(index * 2), Duration: 2}}
	}
	got := window.plan(windows, nil, timeline, 0, 100, false)
	if want := (materializationPlan{begin: 15, end: 30}); got != want {
		t.Fatalf("adaptive progressive plan = %+v, want %+v", got, want)
	}
	for index := 15; index < 30; index++ {
		windows[index] = []ffmpeg.HLSFragment{{Start: float64(index * 2), Duration: 2}}
	}
	if got := window.plan(windows, nil, timeline, 0, 100, false); got != (materializationPlan{begin: 30, end: 45, background: true}) {
		t.Fatalf("plan after urgent reserve = %+v, want background cache fill", got)
	}
	for index := 30; index < 60; index++ {
		windows[index] = []ffmpeg.HLSFragment{{Start: float64(index * 2), Duration: 2}}
	}
	if got := window.plan(windows, nil, timeline, 0, 100, false); got.end > got.begin {
		t.Fatalf("plan beyond byte window = %+v, want no work", got)
	}
}

func TestDirectPrefetchAdvancesOnlyForTailAsset(t *testing.T) {
	stream := &streamInfo{
		materializedWindows: map[int][]ffmpeg.HLSFragment{
			1: {
				{Start: 2, Duration: 2, Name: "direct_000001_0000.m4s"},
				{Start: 4, Duration: 4, Name: "direct_000001_0001.m4s"},
			},
		},
	}
	timeline := newPlaybackTimeline(2*time.Second, 15, 0)
	if got := directPrefetchIndexLocked(stream, timeline, 1, "init_000001.mp4"); got != 1 {
		t.Fatalf("init prefetch index = %d, want 1", got)
	}
	if got := directPrefetchIndexLocked(stream, timeline, 1, "direct_000001_0000.m4s"); got != 1 {
		t.Fatalf("non-tail prefetch index = %d, want 1", got)
	}
	if got := directPrefetchIndexLocked(stream, timeline, 1, "direct_000001_0001.m4s"); got != 3 {
		t.Fatalf("tail prefetch index = %d, want 3", got)
	}
}

func TestProgressivePrefetchDoesNotTreatBufferedVideoAsThePlayhead(t *testing.T) {
	stream := &streamInfo{
		playheadSegment:  10,
		progressiveRetry: true,
	}
	manager := &manager{settings: settings.NewSettings()}

	manager.prefetchProgressiveWindow(stream, 20)

	if stream.playheadSegment != 10 {
		t.Fatalf("playhead segment = %d, want 10", stream.playheadSegment)
	}
}

func TestSelectedRenditionsAdvanceTheDeliveryPlayheadTogether(t *testing.T) {
	stream := &streamInfo{
		mediaInfo:               domain.MediaInfo{Subtitles: []domain.SubtitleTrack{{Index: 0, Codec: "subrip"}}},
		selection:               ffmpeg.StreamSelection{SubtitleTrackIndex: 0},
		playheadSegment:         19,
		videoDeliverySegment:    19,
		subtitleDeliverySegment: 19,
	}

	if got := recordVideoDeliveryLocked(stream, 30); got != 19 {
		t.Fatalf("video-only delivery advanced playhead to %d, want 19", got)
	}
	if got := recordSubtitleDeliveryLocked(stream, 20); got != 20 {
		t.Fatalf("synchronized delivery playhead = %d, want 20", got)
	}
	if got := recordSubtitleDeliveryLocked(stream, 25); got != 25 {
		t.Fatalf("synchronized delivery playhead = %d, want 25", got)
	}
}

func TestDirectFragmentsReportTheRequestedTimeReady(t *testing.T) {
	windows := map[int][]ffmpeg.HLSFragment{
		101: {{Start: 603, Duration: 8, Name: "direct.m4s"}},
		0:   {{Start: 0.083, Duration: 13.5, Name: "first.ts"}},
	}
	if !directFragmentsCoverTime(windows, 0) {
		t.Fatal("initial timestamp offset left source time zero waiting")
	}
	if !directFragmentsCoverTime(windows, 606) {
		t.Fatal("materialized direct fragment did not cover its requested time")
	}
	if directFragmentsCoverTime(windows, 612) {
		t.Fatal("direct fragment covered time outside its range")
	}
}

func TestMaterializedPresentationExcludesDisjointCachedWindows(t *testing.T) {
	windows := map[int][]ffmpeg.HLSFragment{
		0:   {{Start: 0, Duration: 30, Name: "start.ts"}},
		193: {{Start: 386, Duration: 30, Name: "middle.ts"}},
		660: {{Start: 1320, Duration: 30, Name: "target.ts"}},
	}
	fragments := materializedFragmentsForTarget(windows, 1320)
	if len(fragments) != 1 || fragments[0].Name != "target.ts" {
		t.Fatalf("target presentation = %#v", fragments)
	}
	if origin, ok := materializedPresentationOrigin(windows, 1320); !ok || origin != 1320 {
		t.Fatalf("target origin = %.3f, %t", origin, ok)
	}
}

func TestPublishCachedTargetRebuildsOnlyItsContinuousPresentation(t *testing.T) {
	dir := t.TempDir()
	stream := &streamInfo{
		paths: streamPaths{
			outDir:           dir,
			videoPlaylist:    filepath.Join(dir, "index.m3u8"),
			subtitlePlaylist: filepath.Join(dir, "subs.m3u8"),
			masterPlaylist:   filepath.Join(dir, "master.m3u8"),
		},
		assetVersion:       "old",
		directPlay:         true,
		selection:          ffmpeg.StreamSelection{AudioTrackIndex: -1, SubtitleTrackIndex: -1},
		mediaInfo:          domain.MediaInfo{Duration: 300},
		presentationTarget: 0,
		materializedWindows: map[int][]ffmpeg.HLSFragment{
			0:   {{Start: 0, Duration: 30, Name: "start.ts"}},
			100: {{Start: 200, Duration: 30, Name: "target.ts"}},
		},
	}
	manager := &manager{settings: settings.NewSettings(), media: &mediaCache{}}
	target := manager.timeline(stream.mediaInfo.Duration).locate(220)
	if err := manager.publishCachedTarget(stream, target); err != nil {
		t.Fatal(err)
	}
	playlist, err := os.ReadFile(stream.paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(playlist), "start.ts") || !strings.Contains(string(playlist), "target.ts") {
		t.Fatalf("cached target presentation:\n%s", playlist)
	}
	if stream.presentationTarget != 220 || stream.playheadSegment != target.segment || stream.playlistSequence != target.segment || stream.assetVersion == "old" {
		t.Fatalf("cached target state = target %.1f playhead %d sequence %d version %q", stream.presentationTarget, stream.playheadSegment, stream.playlistSequence, stream.assetVersion)
	}
	generation := stream.assetVersion
	if err := manager.publishCachedTarget(stream, target); err != nil {
		t.Fatal(err)
	}
	if stream.assetVersion != generation {
		t.Fatalf("idempotent cached target changed generation: %q != %q", stream.assetVersion, generation)
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
}

func TestReserveJobSlotEnforcesGlobalAndPerStreamLimits(t *testing.T) {
	scheduler := newSegmentScheduler(1, 1, 1)
	stream := &streamInfo{videoJobs: make(map[*segmentJob]struct{}), subtitleJobs: make(map[*segmentJob]struct{})}
	release, err := stream.reserveJobLocked(scheduler, 3, false, func() {})
	if err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if _, err := stream.reserveJobLocked(scheduler, 3, false, func() {}); !errors.Is(err, errStreamJobQueueFull) {
		t.Fatalf("second reservation = %v, want queue full", err)
	}
	release()
	for index := 0; index < 3; index++ {
		stream.videoJobs[&segmentJob{}] = struct{}{}
	}
	if _, err := stream.reserveJobLocked(scheduler, 3, false, func() {}); !errors.Is(err, errStreamJobLimit) {
		t.Fatalf("per-stream reservation = %v, want stream limit", err)
	}
}

func TestSelectedSubtitleJobFailureFailsItsPlaybackTarget(t *testing.T) {
	job := &segmentJob{begin: 0, end: 1, done: make(chan struct{})}
	stream := &streamInfo{
		status:        domain.HlsStatus{Phase: "preparing"},
		statusSegment: 0,
		subtitleJobs:  map[*segmentJob]struct{}{job: {}},
		segmentErrors: make(map[int]segmentFailure),
	}
	stream.markJobError(job, errors.New("ffmpeg failed"))

	if stream.status.Phase != domain.HlsPhaseError || stream.status.Message == "" {
		t.Fatalf("status = %+v", stream.status)
	}
	if job.stage != domain.HlsStageError {
		t.Fatalf("job stage = %q, want error", job.stage)
	}
	if _, ok := stream.segmentErrors[0]; !ok {
		t.Fatalf("selected subtitle failure was not recorded: %+v", stream.segmentErrors)
	}
}

func TestRequestedSubtitleWaitsForTargetVideoWithoutStoppingItsRemainder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer assets.Close()

	scheduler := newSegmentScheduler(3, 1, 1)
	holdCtx, releaseWorker := context.WithCancel(context.Background())
	workerHeld := make(chan struct{})
	go func() {
		_ = scheduler.packageMedia(holdCtx, false, releaseWorker, func() error {
			close(workerHeld)
			<-holdCtx.Done()
			return holdCtx.Err()
		})
	}()
	<-workerHeld
	defer releaseWorker()

	streamCtx, closeStream := context.WithCancel(context.Background())
	foreground := &segmentJob{
		begin:    0,
		end:      8,
		done:     make(chan struct{}),
		progress: make(chan struct{}, 1),
	}
	stream := &streamInfo{
		ctx:                 streamCtx,
		paths:               streamPaths{outDir: root},
		mediaInfo:           domain.MediaInfo{Duration: 120, Subtitles: []domain.SubtitleTrack{{Index: 0, Codec: "subrip"}}},
		selection:           ffmpeg.StreamSelection{SubtitleTrackIndex: 0},
		materializedWindows: make(map[int][]ffmpeg.HLSFragment),
		videoJobs:           map[*segmentJob]struct{}{foreground: {}},
		subtitleJobs:        make(map[*segmentJob]struct{}),
		segmentErrors:       make(map[int]segmentFailure),
	}
	manager := &manager{
		media:     &mediaCache{assets: assets, budget: newCacheBudget(0)},
		scheduler: scheduler,
		settings:  settings.NewSettings(),
	}
	requestDone := make(chan error, 1)
	go func() {
		requestDone <- manager.ensureSubtitleSegment(context.Background(), stream, 7)
	}()
	time.Sleep(150 * time.Millisecond)
	stream.mtx.Lock()
	if len(stream.subtitleJobs) != 0 {
		stream.mtx.Unlock()
		t.Fatal("subtitle job was admitted before its target video")
	}
	stream.materializedWindows[7] = []ffmpeg.HLSFragment{{Start: 14, Duration: 2, Name: videoSegmentName(7)}}
	stream.mtx.Unlock()
	foreground.notifyProgress()

	deadline := time.Now().Add(time.Second)
	var job *segmentJob
	for time.Now().Before(deadline) {
		stream.mtx.Lock()
		for candidate := range stream.subtitleJobs {
			job = candidate
			break
		}
		stream.mtx.Unlock()
		if job != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if job == nil {
		t.Fatal("requested subtitle job was not admitted")
	}
	if job.begin != 7 || job.end != 8 || job.background {
		t.Fatalf("subtitle job = [%d,%d) background=%t, want required foreground [7,8)", job.begin, job.end, job.background)
	}
	stream.mtx.Lock()
	_, videoActive := stream.videoJobs[foreground]
	stream.mtx.Unlock()
	if !videoActive {
		t.Fatal("requested subtitle stopped the video cache remainder")
	}

	closeStream()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("subtitle request did not stop with its stream")
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
	base := domain.HlsStatus{Phase: "preparing", Stage: domain.HlsStageWaitingSource, LastProgress: now.Add(-20 * time.Second)}
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
	base.LastProgress = now.Add(-20 * time.Second)
	base.ActivePeers = 0
	base.Stage = domain.HlsStageWaitingCPU
	if waitingCPU := classifyHlsStatus(base, now); waitingCPU.Phase != "preparing" {
		t.Fatalf("waiting CPU phase = %q, want preparing", waitingCPU.Phase)
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
		begin:                5,
		materializationBegin: 5,
		result:               ffmpeg.VideoWindowResult{Generated: 2, Durations: []float64{6, 2}, ReachedEnd: true},
	}
	manager := &manager{}
	if err := manager.reconcileKnownDuration(stream, job); err != nil {
		t.Fatal(err)
	}
	if stream.mediaInfo.Duration != 18 || stream.status.Duration != 18 {
		t.Fatalf("durations = media %.1f status %.1f", stream.mediaInfo.Duration, stream.status.Duration)
	}
}

func TestPublishingMaterializedWindowKeepsHistoryUntilCachePressure(t *testing.T) {
	dir := t.TempDir()
	stream := &streamInfo{
		paths: streamPaths{
			outDir:           dir,
			videoPlaylist:    filepath.Join(dir, "index.m3u8"),
			subtitlePlaylist: filepath.Join(dir, "subs.m3u8"),
			masterPlaylist:   filepath.Join(dir, "master.m3u8"),
		},
		assetVersion:           "current",
		directPlay:             true,
		materializedWindows:    make(map[int][]ffmpeg.HLSFragment),
		playlistTargetDuration: 30 * time.Second,
		mediaInfo:              domain.MediaInfo{Duration: 300},
	}
	for owner := 0; owner < 3; owner++ {
		stream.materializedWindows[owner] = []ffmpeg.HLSFragment{{
			Start:    float64(owner * 30),
			Duration: 30,
			Name:     videoSegmentName(owner),
		}}
	}
	manager := &manager{settings: settings.NewSettings()}
	job := &segmentJob{begin: 3, materializationBegin: 3}
	stream.videoJobs = map[*segmentJob]struct{}{job: {}}
	fragments := []ffmpeg.HLSFragment{{Start: 90, Duration: 30, Name: videoSegmentName(3)}}
	if err := manager.publishMaterializedWindow(stream, job, fragments, false, true, 4, false); err != nil {
		t.Fatal(err)
	}
	if len(stream.materializedWindows) != 4 {
		t.Fatalf("materialized history = %d windows, want 4", len(stream.materializedWindows))
	}
	if _, ok := stream.materializedWindows[0]; !ok {
		t.Fatal("oldest materialized window was removed without cache pressure")
	}
}

func TestPublishingLargerTargetDurationKeepsMaterializedHistory(t *testing.T) {
	dir := t.TempDir()
	stream := &streamInfo{
		paths: streamPaths{
			outDir:           dir,
			videoPlaylist:    filepath.Join(dir, "index.m3u8"),
			subtitlePlaylist: filepath.Join(dir, "subs.m3u8"),
			masterPlaylist:   filepath.Join(dir, "master.m3u8"),
		},
		assetVersion:           "current",
		directPlay:             true,
		selection:              ffmpeg.StreamSelection{AudioTrackIndex: -1, SubtitleTrackIndex: -1},
		playlistTargetDuration: 2 * time.Second,
		mediaInfo:              domain.MediaInfo{Duration: 300},
		materializedWindows: map[int][]ffmpeg.HLSFragment{
			0: {{Start: 0, Duration: 2, Name: "first.m4s"}},
		},
	}
	manager := &manager{settings: settings.NewSettings()}
	job := &segmentJob{begin: 100, materializationBegin: 100, targetSeconds: 200}
	stream.videoJobs = map[*segmentJob]struct{}{job: {}}
	fragments := []ffmpeg.HLSFragment{{Start: 200, Duration: 8, Name: "distant.m4s"}}
	if err := manager.publishMaterializedWindow(stream, job, fragments, true, true, 101, false); err != nil {
		t.Fatal(err)
	}
	if len(stream.materializedWindows) != 2 || len(stream.materializedWindows[0]) != 1 {
		t.Fatalf("rotated materialized history = %#v", stream.materializedWindows)
	}
	if stream.assetVersion != "current" {
		t.Fatalf("forward seek generation = %q, want current", stream.assetVersion)
	}
	playlist, err := os.ReadFile(stream.paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(playlist), "first.m4s") || !strings.Contains(string(playlist), "distant.m4s") {
		t.Fatalf("active presentation contains disjoint cache history:\n%s", playlist)
	}
}

func TestExtendingCurrentPresentationDoesNotRotateGeneration(t *testing.T) {
	dir := t.TempDir()
	stream := &streamInfo{
		paths: streamPaths{
			outDir:           dir,
			videoPlaylist:    filepath.Join(dir, "index.m3u8"),
			subtitlePlaylist: filepath.Join(dir, "subs.m3u8"),
			masterPlaylist:   filepath.Join(dir, "master.m3u8"),
		},
		assetVersion:           "current",
		directPlay:             true,
		selection:              ffmpeg.StreamSelection{AudioTrackIndex: -1, SubtitleTrackIndex: -1},
		playlistTargetDuration: 2 * time.Second,
		mediaInfo:              domain.MediaInfo{Duration: 300},
		materializedWindows: map[int][]ffmpeg.HLSFragment{
			0: {{Start: 0, Duration: 2, Name: "first.m4s"}},
		},
	}
	manager := &manager{settings: settings.NewSettings()}
	job := &segmentJob{begin: 1, materializationBegin: 1, targetSeconds: 0}
	stream.videoJobs = map[*segmentJob]struct{}{job: {}}
	extension := []ffmpeg.HLSFragment{{Start: 2, Duration: 8, Name: "extension.m4s"}}
	if err := manager.publishMaterializedWindow(stream, job, extension, true, true, 5, false); err != nil {
		t.Fatal(err)
	}
	if stream.assetVersion != "current" {
		t.Fatalf("generation = %q, want current", stream.assetVersion)
	}
	playlist, err := os.ReadFile(stream.paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(playlist), "first.m4s?v=current") || !strings.Contains(string(playlist), "extension.m4s?v=current") {
		t.Fatalf("extended presentation is incomplete:\n%s", playlist)
	}
}

func TestAdjacentTranscodeWindowsKeepContinuousPlaylistTimeline(t *testing.T) {
	dir := t.TempDir()
	first := ffmpeg.HLSFragment{
		Start:    0,
		Duration: 2,
		Name:     videoSegmentName(0),
	}
	stream := &streamInfo{
		paths: streamPaths{
			outDir:           dir,
			videoPlaylist:    filepath.Join(dir, "index.m3u8"),
			subtitlePlaylist: filepath.Join(dir, "subs.m3u8"),
			masterPlaylist:   filepath.Join(dir, "master.m3u8"),
		},
		assetVersion:           "current",
		directPlay:             false,
		selection:              ffmpeg.StreamSelection{AudioTrackIndex: -1, SubtitleTrackIndex: -1},
		playlistTargetDuration: 2 * time.Second,
		mediaInfo:              domain.MediaInfo{Duration: 120},
		materializedWindows: map[int][]ffmpeg.HLSFragment{
			0: {first},
		},
		publishedFragments: []ffmpeg.HLSFragment{first},
	}
	manager := &manager{settings: settings.NewSettings()}
	job := &segmentJob{begin: 1, materializationBegin: 1, targetSeconds: 0}
	stream.videoJobs = map[*segmentJob]struct{}{job: {}}

	continuation := []ffmpeg.HLSFragment{{
		Start:    2,
		Duration: 2,
		Name:     videoSegmentName(1),
	}}
	if err := manager.publishMaterializedWindow(stream, job, continuation, false, false, 2, false); err != nil {
		t.Fatal(err)
	}

	if got := stream.materializedWindows[1][0].Discontinuity; got {
		t.Fatal("continuous transcode window was marked discontinuous")
	}
	playlist, err := os.ReadFile(stream.paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(playlist), "\n#EXT-X-DISCONTINUITY\n") {
		t.Fatalf("continuous transcode playlist contains a discontinuity:\n%s", playlist)
	}
}

func TestDirectPrerollDoesNotRepublishPreviousWindowHead(t *testing.T) {
	dir := t.TempDir()
	first := ffmpeg.HLSFragment{
		Start:    0,
		Duration: 2,
		Name:     "direct_000000_0000.ts",
	}
	stream := &streamInfo{
		paths: streamPaths{
			outDir:           dir,
			videoPlaylist:    filepath.Join(dir, "index.m3u8"),
			subtitlePlaylist: filepath.Join(dir, "subs.m3u8"),
			masterPlaylist:   filepath.Join(dir, "master.m3u8"),
		},
		assetVersion:           "current",
		directPlay:             true,
		selection:              ffmpeg.StreamSelection{AudioTrackIndex: -1, SubtitleTrackIndex: -1},
		playlistTargetDuration: 2 * time.Second,
		mediaInfo:              domain.MediaInfo{Duration: 300},
		materializedWindows:    map[int][]ffmpeg.HLSFragment{0: {first}},
		publishedFragments:     []ffmpeg.HLSFragment{first},
	}
	manager := &manager{settings: settings.NewSettings()}
	job := &segmentJob{begin: 0, end: 16, materializationBegin: 1, targetSeconds: 0}
	stream.videoJobs = map[*segmentJob]struct{}{job: {}}

	preroll := []ffmpeg.HLSFragment{{
		Start:    0,
		Duration: 2,
		Name:     "direct_000001_0000.ts",
	}}
	if err := manager.publishMaterializedWindow(stream, job, preroll, false, true, 1, false); err != nil {
		t.Fatal(err)
	}
	if _, exists := stream.materializedWindows[1]; exists {
		t.Fatalf("preroll fragment was materialized as new media: %#v", stream.materializedWindows)
	}

	continuation := []ffmpeg.HLSFragment{{
		Start:    2,
		Duration: 2,
		Name:     "direct_000001_0001.ts",
	}}
	if err := manager.publishMaterializedWindow(stream, job, continuation, false, true, 2, false); err != nil {
		t.Fatal(err)
	}
	if got := stream.materializedWindows[1]; len(got) != 1 || got[0].Name != continuation[0].Name {
		t.Fatalf("continuation window = %#v", got)
	}
	if len(stream.publishedFragments) != 2 || stream.publishedFragments[0].Name != first.Name ||
		stream.publishedFragments[1].Name != continuation[0].Name {
		t.Fatalf("published fragments = %#v", stream.publishedFragments)
	}
	playlist, err := os.ReadFile(stream.paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(playlist), "#EXTINF:") != 2 ||
		strings.Contains(string(playlist), preroll[0].Name) ||
		strings.Count(string(playlist), "#EXT-X-DISCONTINUITY\n") != 1 {
		t.Fatalf("preroll contaminated the growing presentation:\n%s", playlist)
	}

	job.begin, job.end, job.materializationBegin, job.targetSeconds = 3, 7, 3, 10
	stream.playheadSegment = 5
	before := append([]ffmpeg.HLSFragment(nil), stream.publishedFragments...)
	overlapBeforeTarget := []ffmpeg.HLSFragment{
		{Start: 2, Duration: 2, Name: "duplicate-before-target.ts"},
		{Start: 4, Duration: 2, Name: "bridge-before-target.ts"},
	}
	if err := manager.publishMaterializedWindow(stream, job, overlapBeforeTarget, false, true, 3, false); err != nil {
		t.Fatal(err)
	}
	if len(stream.publishedFragments) != len(before) {
		t.Fatalf("incomplete target replaced the active playlist: %#v", stream.publishedFragments)
	}
	for index := range before {
		if stream.publishedFragments[index] != before[index] {
			t.Fatalf("incomplete target replaced the active playlist: %#v", stream.publishedFragments)
		}
	}
	target := []ffmpeg.HLSFragment{{Start: 6, Duration: 6, Name: "target.ts"}}
	if err := manager.publishMaterializedWindow(stream, job, target, false, true, 7, false); err != nil {
		t.Fatal(err)
	}
	if !fragmentsCoverTime(stream.publishedFragments, 10) {
		t.Fatalf("completed target was not published: %#v", stream.publishedFragments)
	}
}

func TestMaterializedDiskHorizonPublishesOnlyShortPlaylistWindow(t *testing.T) {
	dir := t.TempDir()
	stream := &streamInfo{
		paths: streamPaths{
			outDir:           dir,
			videoPlaylist:    filepath.Join(dir, "index.m3u8"),
			subtitlePlaylist: filepath.Join(dir, "subs.m3u8"),
			masterPlaylist:   filepath.Join(dir, "master.m3u8"),
		},
		assetVersion:           "current",
		directPlay:             true,
		selection:              ffmpeg.StreamSelection{AudioTrackIndex: -1, SubtitleTrackIndex: -1},
		playlistTargetDuration: 2 * time.Second,
		mediaInfo:              domain.MediaInfo{Duration: 300, Bitrate: 1_000_000},
		presentationTarget:     40,
		playheadSegment:        20,
		playlistAnchor:         20,
		materializedWindows:    make(map[int][]ffmpeg.HLSFragment),
		materializedBytes:      make(map[int]int64),
	}
	for owner := 0; owner < 30; owner++ {
		stream.materializedWindows[owner] = []ffmpeg.HLSFragment{{
			Start:    float64(owner * 2),
			Duration: 2,
			Name:     videoSegmentName(owner),
		}}
		stream.materializedBytes[owner] = 1
	}
	manager := &manager{settings: settings.NewSettings()}
	job := &segmentJob{begin: 30, materializationBegin: 30, targetSeconds: 40}
	stream.videoJobs = map[*segmentJob]struct{}{job: {}}
	fragment := []ffmpeg.HLSFragment{{Start: 60, Duration: 2, Name: videoSegmentName(30)}}
	if err := manager.publishMaterializedWindow(stream, job, fragment, false, true, 31, false); err != nil {
		t.Fatal(err)
	}
	if len(stream.materializedWindows) != 31 {
		t.Fatalf("materialized disk horizon = %d windows, want 31", len(stream.materializedWindows))
	}
	if len(stream.publishedFragments) != manager.settings.HlsWindowSegments() {
		t.Fatalf("published playlist = %d fragments, want %d", len(stream.publishedFragments), manager.settings.HlsWindowSegments())
	}
	playlist, err := os.ReadFile(stream.paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(playlist), "#EXTINF:") != manager.settings.HlsWindowSegments() {
		t.Fatalf("playlist exposed disk horizon:\n%s", playlist)
	}
}

func TestDirectFragmentsCanBeAddedBeforeExistingHistory(t *testing.T) {
	windows := map[int][]ffmpeg.HLSFragment{
		100: {{Start: 200, Duration: 8, Name: "later.m4s"}},
	}
	incoming := []ffmpeg.HLSFragment{{Start: 0, Duration: 6, Name: "first.m4s"}}
	got := appendableDirectFragments(windows, incoming)
	if len(got) != 1 || got[0].Name != "first.m4s" {
		t.Fatalf("earlier disjoint fragments = %#v", got)
	}
	if duplicate := appendableDirectFragments(windows, []ffmpeg.HLSFragment{{Start: 201, Duration: 2, Name: "duplicate.m4s"}}); len(duplicate) != 0 {
		t.Fatalf("overlapping fragments = %#v, want none", duplicate)
	}
}

func TestDirectOverlapBridgeAdvancesMaterializedCoverage(t *testing.T) {
	timeline := newPlaybackTimeline(2*time.Second, 15, 120)
	windows := map[int][]ffmpeg.HLSFragment{
		16: {
			{Start: 57.84, Duration: 2.035, Name: "previous-17.ts"},
			{Start: 59.802667, Duration: 2.072333, Name: "previous-18.ts"},
		},
	}
	incoming := []ffmpeg.HLSFragment{
		{Start: 59.375, Duration: 1.958333, Name: "preroll.ts"},
		{Start: 61.258667, Duration: 2.116333, Name: "bridge.ts"},
		{Start: 63.306667, Duration: 2.068333, Name: "continuation.ts"},
	}

	added := appendableDirectFragments(windows, incoming)
	if len(added) != 2 || added[0].Name != "bridge.ts" || added[1].Name != "continuation.ts" {
		t.Fatalf("appendable overlap bridge = %#v", added)
	}
	windows[31] = added
	if begin := nextUncoveredDirectSegment(windows, timeline, 31, 46); begin != 32 {
		t.Fatalf("next uncovered segment = %d, want 32", begin)
	}
}

func TestContiguousTranscodedFragmentsResumeAfterPublishedPrefix(t *testing.T) {
	fragments := []ffmpeg.HLSFragment{
		{Start: 16, Duration: 2, Name: videoSegmentName(8)},
		{Start: 12, Duration: 2, Name: videoSegmentName(6)},
		{Start: 10, Duration: 2, Name: videoSegmentName(5)},
	}
	contiguous, next := contiguousTranscodedFragments(fragments, 5, 10)
	if next != 7 {
		t.Fatalf("resume segment = %d, want 7", next)
	}
	if len(contiguous) != 2 || contiguous[0].Name != videoSegmentName(5) || contiguous[1].Name != videoSegmentName(6) {
		t.Fatalf("published prefix = %#v", contiguous)
	}
	result := transcodedWindowResult(contiguous, false)
	if result.Generated != 2 || len(result.Durations) != 2 || result.Durations[0] != 2 || result.Durations[1] != 2 {
		t.Fatalf("resumed result = %#v", result)
	}
}

func TestDirectWindowPublicationExtendsAnExistingOwner(t *testing.T) {
	dir := t.TempDir()
	stream := &streamInfo{
		paths: streamPaths{
			outDir:           dir,
			videoPlaylist:    filepath.Join(dir, "index.m3u8"),
			subtitlePlaylist: filepath.Join(dir, "subs.m3u8"),
			masterPlaylist:   filepath.Join(dir, "master.m3u8"),
		},
		assetVersion:        "current",
		directPlay:          true,
		presentationTarget:  6,
		mediaInfo:           domain.MediaInfo{Duration: 300},
		materializedWindows: map[int][]ffmpeg.HLSFragment{3: {{Start: 6, Duration: 2, Name: "existing.m4s"}}},
	}
	manager := &manager{settings: settings.NewSettings()}
	job := &segmentJob{begin: 3, materializationBegin: 3, targetSeconds: 6}
	stream.videoJobs = map[*segmentJob]struct{}{job: {}}
	extension := []ffmpeg.HLSFragment{
		{Start: 6, Duration: 2, Name: "duplicate.m4s"},
		{Start: 8, Duration: 2, Name: "extension.m4s"},
	}
	if err := manager.publishMaterializedWindow(stream, job, extension, true, true, 5, false); err != nil {
		t.Fatal(err)
	}
	if len(stream.materializedWindows) != 1 || len(stream.materializedWindows[3]) != 2 ||
		stream.materializedWindows[3][0].Name != "existing.m4s" || stream.materializedWindows[3][1].Name != "extension.m4s" {
		t.Fatalf("materialized windows = %#v", stream.materializedWindows)
	}
}

func TestNextUncoveredDirectSegmentRequiresTheWholeSegment(t *testing.T) {
	timeline := newPlaybackTimeline(2*time.Second, 15, 120)
	windows := map[int][]ffmpeg.HLSFragment{
		0: {{Start: 0, Duration: 2.1, Name: "first.m4s"}},
	}
	if begin := nextUncoveredDirectSegment(windows, timeline, 0, 15); begin != 1 {
		t.Fatalf("next uncovered segment = %d, want 1", begin)
	}
	windows[0][0].Duration = 4
	if begin := nextUncoveredDirectSegment(windows, timeline, 0, 15); begin != 2 {
		t.Fatalf("next uncovered segment = %d, want 2", begin)
	}
}

func TestNextUncoveredDirectSegmentAcceptsContiguousFragmentCoverage(t *testing.T) {
	timeline := newPlaybackTimeline(2*time.Second, 15, 120)
	windows := map[int][]ffmpeg.HLSFragment{
		0: {
			{Start: 0, Duration: 0.9, Name: "first.m4s"},
			{Start: 0.9, Duration: 1.2, Name: "second.m4s"},
		},
	}
	if begin := nextUncoveredDirectSegment(windows, timeline, 0, 15); begin != 1 {
		t.Fatalf("next uncovered segment = %d, want 1", begin)
	}

	windows[0][1].Start = 1.3
	if begin := nextUncoveredDirectSegment(windows, timeline, 0, 15); begin != 0 {
		t.Fatalf("next uncovered segment with a gap = %d, want 0", begin)
	}
}

func TestDirectPrerollBudgetUsesAvailableBytesAndBitrate(t *testing.T) {
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", "536870912")
	t.Setenv("CINEMATOR_MAX_QUEUED_JOBS", "4")
	manager := &manager{settings: settings.NewSettings()}
	lowBitrate := manager.directPrerollBudget(5_500_000)
	highBitrate := manager.directPrerollBudget(50_000_000)
	if lowBitrate <= highBitrate || lowBitrate <= 30*time.Second {
		t.Fatalf("preroll budgets = low %s, high %s", lowBitrate, highBitrate)
	}
}

func TestReconcileKnownDurationUsesDirectSourceTimeline(t *testing.T) {
	stream := &streamInfo{
		mediaInfo: domain.MediaInfo{Duration: 120, Seekable: true},
		status:    domain.HlsStatus{Duration: 120, Seekable: true},
	}
	job := &segmentJob{fragments: []ffmpeg.HLSFragment{{Start: 29.5, Duration: 7.5}}}
	if err := (&manager{}).reconcileKnownDuration(stream, job); err != nil {
		t.Fatal(err)
	}
	if stream.mediaInfo.Duration != 37 || stream.status.Duration != 37 {
		t.Fatalf("direct durations = media %.1f status %.1f", stream.mediaInfo.Duration, stream.status.Duration)
	}
}
