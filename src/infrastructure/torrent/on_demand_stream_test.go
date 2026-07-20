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
	job := &segmentJob{done: make(chan struct{})}
	go func() {
		time.Sleep(25 * time.Millisecond)
		_ = os.WriteFile(path, []byte("segment"), 0644)
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
	scheduler := newSegmentScheduler(1, 1)
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
	stream.materializedTarget = 12 * time.Second
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
	stream := &streamInfo{ctx: context.Background(), ready: ready, assetVersion: "current", paths: paths, progressiveEnded: true}
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
		paths:                 streamPaths{subtitlePlaylist: playlist},
		assetVersion:          "v1",
		progressiveAdvertised: 1,
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
	for _, test := range []struct {
		name       string
		demand     int
		advertised int
		total      int
		maximum    int
		startup    bool
		want       progressivePlan
	}{
		{
			name:       "startup publishes one segment",
			demand:     100,
			advertised: 100,
			total:      200,
			maximum:    15,
			startup:    true,
			want:       progressivePlan{end: 101, ok: true},
		},
		{
			name:       "foreground fills low watermark",
			demand:     0,
			advertised: 1,
			total:      200,
			maximum:    15,
			want:       progressivePlan{end: 8, ok: true},
		},
		{
			name:       "background fills target watermark",
			demand:     0,
			advertised: 8,
			total:      200,
			maximum:    15,
			want:       progressivePlan{end: 15, background: true, ok: true},
		},
		{
			name:       "target watermark stops prefetch",
			demand:     0,
			advertised: 15,
			total:      200,
			maximum:    15,
			want:       progressivePlan{},
		},
		{
			name:       "tail is capped by duration",
			demand:     95,
			advertised: 99,
			total:      100,
			maximum:    15,
			want:       progressivePlan{end: 100, ok: true},
		},
		{
			name:       "forward seek refills from requested position",
			demand:     660,
			advertised: 661,
			total:      1000,
			maximum:    15,
			want:       progressivePlan{end: 668, ok: true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := progressivePrefetchPlan(test.demand, test.advertised, test.total, test.maximum, 2*time.Second, 30*time.Second, test.startup)
			if got != test.want {
				t.Fatalf("progressivePrefetchPlan() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestStreamDeliveryRiskRaisesAndRecoversTheForwardReserve(t *testing.T) {
	now := time.Now()
	stream := &streamInfo{progressiveTarget: 30 * time.Second}
	stream.recordJobDelivery(&segmentJob{begin: 0, end: 5, startedAt: now.Add(-9 * time.Second)}, 2*time.Second, now)
	if stream.progressiveTarget != 60*time.Second {
		t.Fatalf("target after marginal delivery = %v, want 60s", stream.progressiveTarget)
	}

	for range 12 {
		stream.recordJobDelivery(&segmentJob{begin: 0, end: 5, startedAt: now.Add(-time.Second)}, 2*time.Second, now)
	}
	if stream.progressiveTarget != 30*time.Second {
		t.Fatalf("target after sustained fast delivery = %v, want 30s", stream.progressiveTarget)
	}
}

func TestProgressivePrefetchPlanUsesAdaptiveTargetWithoutExceedingHighWatermark(t *testing.T) {
	got := progressivePrefetchPlan(0, 15, 100, 15, 2*time.Second, 60*time.Second, false)
	if want := (progressivePlan{end: 30, background: true, ok: true}); got != want {
		t.Fatalf("adaptive progressive plan = %+v, want %+v", got, want)
	}
	if got := progressivePrefetchPlan(0, 30, 100, 15, 2*time.Second, 60*time.Second, false); got.ok {
		t.Fatalf("plan beyond high watermark = %+v, want no work", got)
	}
}

func TestDirectPrefetchAdvancesOnlyForTailAsset(t *testing.T) {
	stream := &streamInfo{
		progressiveAdvertised: 16,
		directWindows: map[int][]ffmpeg.HLSFragment{
			1: {
				{Name: "direct_000001_0000.m4s"},
				{Name: "direct_000001_0001.m4s"},
			},
		},
	}
	if got := directPrefetchIndexLocked(stream, 1, "init_000001.mp4"); got != 1 {
		t.Fatalf("init prefetch index = %d, want 1", got)
	}
	if got := directPrefetchIndexLocked(stream, 1, "direct_000001_0000.m4s"); got != 1 {
		t.Fatalf("non-tail prefetch index = %d, want 1", got)
	}
	if got := directPrefetchIndexLocked(stream, 1, "direct_000001_0001.m4s"); got != 15 {
		t.Fatalf("tail prefetch index = %d, want 15", got)
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
		directWindows: map[int][]ffmpeg.HLSFragment{
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
	if stream.presentationTarget != 220 || stream.progressiveSequence != target.segment || stream.assetVersion == "old" {
		t.Fatalf("cached target state = target %.1f sequence %d version %q", stream.presentationTarget, stream.progressiveSequence, stream.assetVersion)
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
	scheduler := newSegmentScheduler(1, 1)
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
		begin:  5,
		result: ffmpeg.VideoWindowResult{Generated: 2, Durations: []float64{6, 2}, ReachedEnd: true},
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
		assetVersion:       "current",
		directPlay:         true,
		directWindows:      make(map[int][]ffmpeg.HLSFragment),
		materializedTarget: 30 * time.Second,
		mediaInfo:          domain.MediaInfo{Duration: 300},
	}
	for owner := 0; owner < 3; owner++ {
		stream.directWindows[owner] = []ffmpeg.HLSFragment{{
			Start:    float64(owner * 30),
			Duration: 30,
			Name:     videoSegmentName(owner),
		}}
	}
	manager := &manager{settings: settings.NewSettings()}
	job := &segmentJob{begin: 3}
	fragments := []ffmpeg.HLSFragment{{Start: 90, Duration: 30, Name: videoSegmentName(3)}}
	if err := manager.publishMaterializedWindow(stream, job, fragments, false, true, 4, false); err != nil {
		t.Fatal(err)
	}
	if len(stream.directWindows) != 4 {
		t.Fatalf("materialized history = %d windows, want 4", len(stream.directWindows))
	}
	if _, ok := stream.directWindows[0]; !ok {
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
		assetVersion:       "current",
		directPlay:         true,
		selection:          ffmpeg.StreamSelection{AudioTrackIndex: -1, SubtitleTrackIndex: -1},
		materializedTarget: 2 * time.Second,
		mediaInfo:          domain.MediaInfo{Duration: 300},
		directWindows: map[int][]ffmpeg.HLSFragment{
			0: {{Start: 0, Duration: 2, Name: "first.m4s"}},
		},
	}
	manager := &manager{settings: settings.NewSettings()}
	job := &segmentJob{begin: 100, targetSeconds: 200}
	fragments := []ffmpeg.HLSFragment{{Start: 200, Duration: 8, Name: "distant.m4s"}}
	if err := manager.publishMaterializedWindow(stream, job, fragments, true, true, 101, false); err != nil {
		t.Fatal(err)
	}
	if len(stream.directWindows) != 2 || len(stream.directWindows[0]) != 1 {
		t.Fatalf("rotated materialized history = %#v", stream.directWindows)
	}
	playlist, err := os.ReadFile(stream.paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(playlist), "first.m4s") || !strings.Contains(string(playlist), "distant.m4s") {
		t.Fatalf("active presentation contains disjoint cache history:\n%s", playlist)
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

func TestDirectWindowPublicationExtendsAnExistingOwner(t *testing.T) {
	dir := t.TempDir()
	stream := &streamInfo{
		paths: streamPaths{
			outDir:           dir,
			videoPlaylist:    filepath.Join(dir, "index.m3u8"),
			subtitlePlaylist: filepath.Join(dir, "subs.m3u8"),
			masterPlaylist:   filepath.Join(dir, "master.m3u8"),
		},
		assetVersion:       "current",
		directPlay:         true,
		presentationTarget: 6,
		mediaInfo:          domain.MediaInfo{Duration: 300},
		directWindows:      map[int][]ffmpeg.HLSFragment{3: {{Start: 6, Duration: 2, Name: "existing.m4s"}}},
	}
	manager := &manager{settings: settings.NewSettings()}
	job := &segmentJob{begin: 3, targetSeconds: 6}
	extension := []ffmpeg.HLSFragment{
		{Start: 6, Duration: 2, Name: "duplicate.m4s"},
		{Start: 8, Duration: 2, Name: "extension.m4s"},
	}
	if err := manager.publishMaterializedWindow(stream, job, extension, true, true, 5, false); err != nil {
		t.Fatal(err)
	}
	if len(stream.directWindows) != 1 || len(stream.directWindows[3]) != 2 ||
		stream.directWindows[3][0].Name != "existing.m4s" || stream.directWindows[3][1].Name != "extension.m4s" {
		t.Fatalf("materialized windows = %#v", stream.directWindows)
	}
}

func TestNextDirectWindowStartsAfterMaterializedSourceCoverage(t *testing.T) {
	windows := map[int][]ffmpeg.HLSFragment{
		95: {
			{Start: 590, Duration: 20},
			{Start: 610, Duration: 19.5},
		},
	}
	timeline := newPlaybackTimeline(6*time.Second, 5, 0)
	if begin := nextDirectWindowBegin(windows, timeline); begin != 104 {
		t.Fatalf("next window begin = %d, want 104", begin)
	}
	windows[95][1].Duration = 20
	if begin := nextDirectWindowBegin(windows, timeline); begin != 105 {
		t.Fatalf("aligned next window begin = %d, want 105", begin)
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
