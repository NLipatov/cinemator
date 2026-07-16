package torrent

import (
	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
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
	stream.recordSourceBytes(8192)

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
