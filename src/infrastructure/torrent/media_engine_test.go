package torrent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
	"cinemator/presentation/settings"
)

type fakeMediaEngine struct {
	videoRequests    []videoWindowRequest
	videoDurations   []float64
	directRequests   []directWindowRequest
	subtitleRequests []subtitleSegmentRequest
	tailRequests     []tailProbeRequest
	tailDuration     float64
}

type tailProbeRequest struct {
	inputURL   string
	videoTrack int
}

func (f *fakeMediaEngine) Probe(context.Context, io.Reader) (domain.MediaInfo, error) {
	panic("unexpected Probe call")
}

func (f *fakeMediaEngine) ProbeURL(context.Context, string) (domain.MediaInfo, error) {
	panic("unexpected ProbeURL call")
}

func (f *fakeMediaEngine) ProbeTailDuration(_ context.Context, inputURL string, videoTrack int) (float64, error) {
	f.tailRequests = append(f.tailRequests, tailProbeRequest{
		inputURL:   inputURL,
		videoTrack: videoTrack,
	})
	return f.tailDuration, nil
}

func (f *fakeMediaEngine) GenerateVideoWindow(
	_ context.Context,
	request videoWindowRequest,
	onPublished func(int, float64) error,
) (ffmpeg.VideoWindowResult, error) {
	f.videoRequests = append(f.videoRequests, request)
	durations := make([]float64, 0, request.SegmentCount)
	for index := request.FirstSegment; index < request.FirstSegment+request.SegmentCount; index++ {
		if err := os.WriteFile(
			filepath.Join(request.OutputDir, videoSegmentName(index)),
			[]byte("fake transport stream"),
			0600,
		); err != nil {
			return ffmpeg.VideoWindowResult{}, err
		}
		duration := request.SegmentDuration.Seconds()
		offset := index - request.FirstSegment
		if offset < len(f.videoDurations) {
			duration = f.videoDurations[offset]
		}
		if err := onPublished(index, duration); err != nil {
			return ffmpeg.VideoWindowResult{}, err
		}
		durations = append(durations, duration)
	}
	return ffmpeg.VideoWindowResult{
		Generated:  request.SegmentCount,
		Durations:  durations,
		ReachedEnd: true,
	}, nil
}

func (f *fakeMediaEngine) GenerateDirectWindow(
	_ context.Context,
	request directWindowRequest,
	onPublished func(ffmpeg.HLSFragment) error,
) (ffmpeg.DirectWindowResult, error) {
	f.directRequests = append(f.directRequests, request)
	fragment := ffmpeg.HLSFragment{
		Start:    float64(request.SourceSegment) * request.SegmentDuration.Seconds(),
		Duration: request.SegmentDuration.Seconds(),
		Name:     videoSegmentName(request.AssetOwner),
	}
	if err := os.WriteFile(
		filepath.Join(request.OutputDir, fragment.Name),
		[]byte("fake transport stream"),
		0600,
	); err != nil {
		return ffmpeg.DirectWindowResult{}, err
	}
	if err := onPublished(fragment); err != nil {
		return ffmpeg.DirectWindowResult{}, err
	}
	return ffmpeg.DirectWindowResult{
		Fragments:  []ffmpeg.HLSFragment{fragment},
		ReachedEnd: true,
	}, nil
}

func (f *fakeMediaEngine) GenerateSubtitleSegment(
	_ context.Context,
	request subtitleSegmentRequest,
) error {
	f.subtitleRequests = append(f.subtitleRequests, request)
	return os.WriteFile(request.OutputPath, []byte("WEBVTT\n\n"), 0600)
}

func TestSubtitleJobUsesMediaEngineBoundary(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer assets.Close()

	engine := &fakeMediaEngine{}
	scheduler := newSegmentScheduler(2, 1, 1)
	streamCtx, closeStream := context.WithCancel(context.Background())
	defer closeStream()
	stream := &streamInfo{
		ctx:           streamCtx,
		paths:         streamPaths{outDir: root},
		source:        &torrentSource{url: "http://media-source.invalid/source"},
		mediaInfo:     domain.MediaInfo{Duration: 120},
		selection:     ffmpeg.StreamSelection{SubtitleTrackIndex: 7},
		videoJobs:     make(map[*segmentJob]struct{}),
		subtitleJobs:  make(map[*segmentJob]struct{}),
		segmentErrors: make(map[int]segmentFailure),
	}
	manager := &manager{
		packager:  engine,
		media:     &mediaCache{assets: assets, budget: newCacheBudget(0)},
		scheduler: scheduler,
		settings:  settings.NewSettings(),
	}

	stream.mtx.Lock()
	job, jobCtx, created, err := stream.acquireJobLocked(
		subtitleSegmentJob,
		3,
		3,
		4,
		false,
		scheduler,
		2,
	)
	stream.mtx.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("subtitle job was not created")
	}

	manager.runSubtitleJob(stream, jobCtx, job)

	if job.err != nil {
		t.Fatalf("subtitle job failed: %v", job.err)
	}
	if len(engine.subtitleRequests) != 1 {
		t.Fatalf("subtitle requests = %d, want 1", len(engine.subtitleRequests))
	}
	request := engine.subtitleRequests[0]
	if request.InputURL != "http://media-source.invalid/source?job="+job.id {
		t.Fatalf("input URL = %q", request.InputURL)
	}
	if request.OutputPath != filepath.Join(root, subtitleSegmentName(3)) {
		t.Fatalf("output path = %q", request.OutputPath)
	}
	if request.SubtitleTrack != 7 || request.SegmentIndex != 3 {
		t.Fatalf("subtitle request = %#v", request)
	}
	if request.SegmentDuration != 2*time.Second {
		t.Fatalf("segment duration = %s", request.SegmentDuration)
	}
	if _, err := os.Stat(request.OutputPath); err != nil {
		t.Fatalf("published subtitle: %v", err)
	}
}

func TestSelectedSubtitleAssetPreemptsBackgroundSessionExecution(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer assets.Close()

	engine := &fakeMediaEngine{}
	scheduler := newSegmentScheduler(4, 1, 1)
	streamCtx, closeStream := context.WithCancel(context.Background())
	defer closeStream()
	stream := &streamInfo{
		ctx:          streamCtx,
		paths:        streamPaths{outDir: root},
		source:       &torrentSource{url: "http://media-source.invalid/source"},
		mediaInfo:    domain.MediaInfo{Duration: 120, Subtitles: []domain.SubtitleTrack{{Codec: "subrip"}}},
		selection:    ffmpeg.StreamSelection{SubtitleTrackIndex: 0},
		videoJobs:    make(map[*segmentJob]struct{}),
		subtitleJobs: make(map[*segmentJob]struct{}),
		materializedWindows: map[int][]ffmpeg.HLSFragment{
			0: {{Start: 0, Duration: 4, Name: videoSegmentName(0)}},
		},
		presentationTarget: 2,
		materializedEnd:    60,
		segmentErrors:      make(map[int]segmentFailure),
		progressiveRetry:   true,
	}
	manager := &manager{
		packager:  engine,
		media:     &mediaCache{assets: assets, budget: newCacheBudget(0)},
		scheduler: scheduler,
		settings:  settings.NewSettings(),
	}

	stream.mtx.Lock()
	background, backgroundCtx, created, err := stream.acquireJobLocked(
		videoSegmentJob,
		10,
		10,
		20,
		true,
		scheduler,
		4,
	)
	stream.mtx.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("background video job was not created")
	}

	backgroundStarted := make(chan struct{})
	backgroundDone := make(chan error, 1)
	go func() {
		background.err = manager.runBoundedPackager(
			backgroundCtx,
			stream,
			background,
			mediaEncoderWorker,
			func(ctx context.Context) error {
				close(backgroundStarted)
				<-ctx.Done()
				return ctx.Err()
			},
		)
		stream.completeJob(videoSegmentJob, background)
		backgroundDone <- background.err
	}()
	select {
	case <-backgroundStarted:
	case <-time.After(time.Second):
		t.Fatal("background video execution did not start")
	}

	requestCtx, cancelRequest := context.WithTimeout(context.Background(), time.Second)
	defer cancelRequest()
	if err := manager.ensureSubtitleSegment(requestCtx, stream, 1); err != nil {
		t.Fatalf("ensure selected subtitle while background video is active: %v", err)
	}
	if len(engine.subtitleRequests) != 1 || engine.subtitleRequests[0].SegmentIndex != 1 {
		t.Fatalf("subtitle requests = %#v", engine.subtitleRequests)
	}
	if _, err := os.Stat(filepath.Join(root, subtitleSegmentName(1))); err != nil {
		t.Fatalf("selected subtitle was not published: %v", err)
	}
	select {
	case err := <-backgroundDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("preempted background execution = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("preempted background video execution did not release the session lane")
	}
}

func TestSelectedSubtitleAssetCreatesMissingForegroundVideoJob(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer assets.Close()

	engine := &fakeMediaEngine{videoDurations: []float64{1.3}}
	scheduler := newSegmentScheduler(2, 1, 1)
	streamCtx, closeStream := context.WithCancel(context.Background())
	defer closeStream()
	key := streamKey{
		InfoHash:  "0123456789abcdef0123456789abcdef01234567",
		Session:   "viewer",
		Index:     0,
		Audio:     0,
		Subtitle:  0,
		Transcode: true,
	}
	paths := key.paths(root)
	if err := os.MkdirAll(paths.outDir, 0700); err != nil {
		t.Fatal(err)
	}
	info := domain.MediaInfo{
		Duration:   120,
		Seekable:   true,
		VideoCodec: "h264",
		Bitrate:    1_000_000,
		Subtitles:  []domain.SubtitleTrack{{Index: 0, Codec: "subrip"}},
	}
	selection := ffmpeg.StreamSelection{
		AudioTrackIndex:    0,
		SubtitleTrackIndex: 0,
		ForceTranscode:     true,
	}
	const generation = "test-generation"
	if err := ffmpeg.PrepareOnDemandHLS(
		paths.outDir,
		paths.videoPlaylist,
		paths.subtitlePlaylist,
		paths.masterPlaylist,
		info,
		selection,
		2*time.Second,
		15,
		generation,
	); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	stream := &streamInfo{
		ctx:                 streamCtx,
		paths:               paths,
		assetVersion:        generation,
		source:              &torrentSource{url: "http://media-source.invalid/source"},
		ready:               ready,
		mediaInfo:           info,
		mediaInfoReady:      true,
		selection:           selection,
		presentationTarget:  41.4,
		playheadSegment:     20,
		directPlay:          false,
		materializedWindows: make(map[int][]ffmpeg.HLSFragment),
		materializedBytes:   make(map[int]int64),
		retainedAssets:      make(map[string]time.Time),
		videoJobs:           make(map[*segmentJob]struct{}),
		subtitleJobs:        make(map[*segmentJob]struct{}),
		segmentErrors:       make(map[int]segmentFailure),
		statusSegment:       -1,
	}
	manager := &manager{
		packager:  engine,
		media:     &mediaCache{assets: assets, budget: newCacheBudget(0)},
		scheduler: scheduler,
		settings:  settings.NewSettings(),
	}

	requestCtx, cancelRequest := context.WithTimeout(context.Background(), time.Second)
	defer cancelRequest()
	if err := manager.ensureSubtitleSegment(requestCtx, stream, 20); err != nil {
		t.Fatalf("ensure selected subtitle without an existing video job: %v", err)
	}
	if len(engine.videoRequests) != 1 {
		t.Fatalf("video requests = %d, want 1", len(engine.videoRequests))
	}
	video := engine.videoRequests[0]
	if video.FirstSegment > 20 || video.FirstSegment+video.SegmentCount <= 20 {
		t.Fatalf("video request %#v does not cover subtitle segment 20", video)
	}
	if len(engine.subtitleRequests) != 1 || engine.subtitleRequests[0].SegmentIndex != 20 {
		t.Fatalf("subtitle requests = %#v", engine.subtitleRequests)
	}
	if _, err := os.Stat(filepath.Join(paths.outDir, subtitleSegmentName(20))); err != nil {
		t.Fatalf("selected subtitle was not published: %v", err)
	}
}

func TestVideoJobUsesMediaEngineBoundary(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer assets.Close()

	engine := &fakeMediaEngine{}
	scheduler := newSegmentScheduler(2, 1, 1)
	streamCtx, closeStream := context.WithCancel(context.Background())
	defer closeStream()
	key := streamKey{
		InfoHash:  "0123456789abcdef0123456789abcdef01234567",
		Session:   "viewer",
		Index:     0,
		Audio:     0,
		Subtitle:  -1,
		Transcode: true,
	}
	paths := key.paths(root)
	if err := os.MkdirAll(paths.outDir, 0700); err != nil {
		t.Fatal(err)
	}
	info := domain.MediaInfo{
		Duration:   2,
		Seekable:   true,
		VideoCodec: "h264",
		Bitrate:    1_000_000,
	}
	selection := ffmpeg.StreamSelection{
		AudioTrackIndex:    0,
		SubtitleTrackIndex: -1,
		ForceTranscode:     true,
	}
	const generation = "test-generation"
	if err := ffmpeg.PrepareOnDemandHLS(
		paths.outDir,
		paths.videoPlaylist,
		paths.subtitlePlaylist,
		paths.masterPlaylist,
		info,
		selection,
		2*time.Second,
		15,
		generation,
	); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	stream := &streamInfo{
		ctx:                 streamCtx,
		paths:               paths,
		assetVersion:        generation,
		source:              &torrentSource{url: "http://media-source.invalid/source"},
		ready:               ready,
		mediaInfo:           info,
		mediaInfoReady:      true,
		selection:           selection,
		presentationTarget:  0,
		directPlay:          false,
		materializedWindows: make(map[int][]ffmpeg.HLSFragment),
		materializedBytes:   make(map[int]int64),
		retainedAssets:      make(map[string]time.Time),
		videoJobs:           make(map[*segmentJob]struct{}),
		subtitleJobs:        make(map[*segmentJob]struct{}),
		segmentErrors:       make(map[int]segmentFailure),
		statusSegment:       -1,
	}
	manager := &manager{
		packager:  engine,
		media:     &mediaCache{assets: assets, budget: newCacheBudget(0)},
		scheduler: scheduler,
		settings:  settings.NewSettings(),
	}

	stream.mtx.Lock()
	job, jobCtx, created, err := stream.acquireJobLocked(
		videoSegmentJob,
		0,
		0,
		1,
		false,
		scheduler,
		2,
	)
	stream.mtx.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("video job was not created")
	}

	manager.runVideoJob(stream, jobCtx, job)

	if job.err != nil {
		t.Fatalf("video job failed: %v", job.err)
	}
	if len(engine.videoRequests) != 1 {
		t.Fatalf("video requests = %d, want 1", len(engine.videoRequests))
	}
	request := engine.videoRequests[0]
	if request.InputURL != "http://media-source.invalid/source?job="+job.id {
		t.Fatalf("input URL = %q", request.InputURL)
	}
	if request.FirstSegment != 0 || request.SegmentCount != 1 {
		t.Fatalf("video request = %#v", request)
	}
	playlist, err := os.ReadFile(paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(playlist), "\n"+videoSegmentName(0)+"?v="+generation+"\n") {
		t.Fatalf("video playlist does not publish generated segment:\n%s", playlist)
	}
}

func TestSwitchToTranscodeRetargetsFallbackJobToActivePresentation(t *testing.T) {
	root := t.TempDir()
	key := streamKey{
		InfoHash:  "0123456789abcdef0123456789abcdef01234567",
		Session:   "viewer",
		Index:     0,
		Audio:     0,
		Subtitle:  -1,
		Transcode: false,
	}
	paths := key.paths(root)
	if err := os.MkdirAll(paths.outDir, 0700); err != nil {
		t.Fatal(err)
	}
	info := domain.MediaInfo{
		Duration:   120,
		Seekable:   true,
		VideoCodec: "h264",
		Bitrate:    1_000_000,
	}
	selection := ffmpeg.StreamSelection{
		AudioTrackIndex:    0,
		SubtitleTrackIndex: -1,
	}
	const generation = "direct-generation"
	if err := ffmpeg.PrepareOnDemandHLS(
		paths.outDir,
		paths.videoPlaylist,
		paths.subtitlePlaylist,
		paths.masterPlaylist,
		info,
		selection,
		2*time.Second,
		15,
		generation,
	); err != nil {
		t.Fatal(err)
	}
	current := &segmentJob{
		begin:                3,
		end:                  6,
		materializationBegin: 3,
		background:           true,
		targetSeconds:        6,
		fragments: []ffmpeg.HLSFragment{{
			Start: 6, Duration: 2, Name: "direct_000003_0000.ts",
		}},
	}
	stream := &streamInfo{
		paths:              paths,
		assetVersion:       generation,
		mediaInfo:          info,
		selection:          selection,
		presentationTarget: 0,
		// HLS clients request ahead before presenting a frame. Delivery demand
		// must not move fallback ownership away from the user's target.
		playheadSegment: 2,
		directPlay:      true,
		materializedWindows: map[int][]ffmpeg.HLSFragment{
			0: {{Start: 0, Duration: 6, Name: "direct_000000_0000.ts"}},
		},
		materializedBytes: map[int]int64{0: 1024},
		retainedAssets:    make(map[string]time.Time),
		videoJobs:         map[*segmentJob]struct{}{current: {}},
	}
	manager := &manager{settings: settings.NewSettings()}

	if err := manager.switchToTranscode(stream, current); err != nil {
		t.Fatal(err)
	}
	if current.begin != 0 || current.materializationBegin != 0 || current.end < 3 {
		t.Fatalf(
			"fallback job remained [%d,%d) materialized at %d, want active target from zero",
			current.begin,
			current.end,
			current.materializationBegin,
		)
	}
	if current.background {
		t.Fatal("fallback job remained background after taking ownership of active playback")
	}
	if current.targetSeconds != 0 {
		t.Fatalf("fallback target = %.3f, want active presentation target 0", current.targetSeconds)
	}
	if len(current.fragments) != 0 || current.result.Generated != 0 || current.directEnd {
		t.Fatalf("fallback job retained direct result: %#v", current)
	}
	if stream.directPlay || !stream.selection.ForceTranscode {
		t.Fatalf("stream mode = direct %t, force transcode %t", stream.directPlay, stream.selection.ForceTranscode)
	}
	if stream.assetVersion == generation {
		t.Fatal("fallback reused the direct presentation generation")
	}
}

func TestTorrentSourceUsesMediaAnalyzerBoundaryForTailProbe(t *testing.T) {
	engine := &fakeMediaEngine{tailDuration: 3_601.25}
	source := &torrentSource{
		analyzer: engine,
		url:      "http://media-source.invalid/source",
	}
	info := domain.MediaInfo{
		Duration:        3_600,
		VideoTrackIndex: 4,
	}

	refined, err := source.refineDurationFromTail(context.Background(), info)
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.tailRequests) != 1 {
		t.Fatalf("tail probe requests = %d, want 1", len(engine.tailRequests))
	}
	request := engine.tailRequests[0]
	if request.inputURL != source.url || request.videoTrack != info.VideoTrackIndex {
		t.Fatalf("tail probe request = %#v", request)
	}
	if refined.Duration != engine.tailDuration || !refined.Seekable {
		t.Fatalf("refined media info = %#v", refined)
	}
}

func TestDirectVideoJobUsesMediaEngineBoundary(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer assets.Close()

	engine := &fakeMediaEngine{}
	scheduler := newSegmentScheduler(2, 1, 1)
	streamCtx, closeStream := context.WithCancel(context.Background())
	defer closeStream()
	key := streamKey{
		InfoHash: "0123456789abcdef0123456789abcdef01234567",
		Session:  "viewer",
		Index:    0,
		Audio:    -1,
		Subtitle: -1,
	}
	paths := key.paths(root)
	if err := os.MkdirAll(paths.outDir, 0700); err != nil {
		t.Fatal(err)
	}
	info := domain.MediaInfo{
		Duration:   2,
		Seekable:   true,
		VideoCodec: "h264",
		Bitrate:    1_000_000,
	}
	selection := ffmpeg.StreamSelection{
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
	}
	const generation = "test-generation"
	if err := ffmpeg.PrepareOnDemandHLS(
		paths.outDir,
		paths.videoPlaylist,
		paths.subtitlePlaylist,
		paths.masterPlaylist,
		info,
		selection,
		2*time.Second,
		15,
		generation,
	); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	stream := &streamInfo{
		ctx:                 streamCtx,
		paths:               paths,
		assetVersion:        generation,
		source:              &torrentSource{url: "http://media-source.invalid/source"},
		ready:               ready,
		mediaInfo:           info,
		mediaInfoReady:      true,
		selection:           selection,
		presentationTarget:  0,
		directPlay:          true,
		materializedWindows: make(map[int][]ffmpeg.HLSFragment),
		materializedBytes:   make(map[int]int64),
		retainedAssets:      make(map[string]time.Time),
		videoJobs:           make(map[*segmentJob]struct{}),
		subtitleJobs:        make(map[*segmentJob]struct{}),
		segmentErrors:       make(map[int]segmentFailure),
		statusSegment:       -1,
	}
	manager := &manager{
		packager:  engine,
		media:     &mediaCache{assets: assets, budget: newCacheBudget(0)},
		scheduler: scheduler,
		settings:  settings.NewSettings(),
	}

	stream.mtx.Lock()
	job, jobCtx, created, err := stream.acquireJobLocked(
		videoSegmentJob,
		0,
		0,
		1,
		false,
		scheduler,
		2,
	)
	stream.mtx.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("direct video job was not created")
	}

	manager.runVideoJob(stream, jobCtx, job)

	if job.err != nil {
		t.Fatalf("direct video job failed: %v", job.err)
	}
	if len(engine.directRequests) != 1 {
		t.Fatalf("direct requests = %d, want 1", len(engine.directRequests))
	}
	request := engine.directRequests[0]
	if request.InputURL != "http://media-source.invalid/source?job="+job.id {
		t.Fatalf("input URL = %q", request.InputURL)
	}
	if request.SourceSegment != 0 || request.AssetOwner != 0 || request.SegmentCount != 1 {
		t.Fatalf("direct request = %#v", request)
	}
	playlist, err := os.ReadFile(paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(playlist), "\n"+videoSegmentName(0)+"?v="+generation+"\n") {
		t.Fatalf("video playlist does not publish direct segment:\n%s", playlist)
	}
}
