package ffmpeg

import (
	"cinemator/domain"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublishFileWithoutReplacementKeepsPublishedAsset(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(source, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("published"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := publishFileWithoutReplacement(source, target); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "published" {
		t.Fatalf("published asset = %q, %v", data, err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary source remains: %v", err)
	}
}

func TestPrepareOnDemandHLSPublishesVirtualTextSubtitleTimeline(t *testing.T) {
	dir := t.TempDir()
	info := domain.MediaInfo{
		Duration:  30,
		Bitrate:   50_000_000,
		Subtitles: []domain.SubtitleTrack{{Codec: "subrip", Language: "eng"}},
	}
	selection := StreamSelection{AudioTrackIndex: 0, SubtitleTrackIndex: 0}

	err := PrepareOnDemandHLS(
		dir,
		dir+"/index.m3u8",
		dir+"/subs.m3u8",
		dir+"/master.m3u8",
		info,
		selection,
		10*time.Second,
		2,
		"v1",
	)
	if err != nil {
		t.Fatalf("PrepareOnDemandHLS() error = %v", err)
	}
	assertFileContains(t, dir+"/master.m3u8", "SUBTITLES=\"subs\"")
	assertFileContains(t, dir+"/master.m3u8", "BANDWIDTH=150000000")
	assertFileContains(t, dir+"/master.m3u8", "index.m3u8?v=v1")
	video, err := os.ReadFile(dir + "/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(video), ".ts") || strings.Contains(string(video), ".m4s") || strings.Contains(string(video), "#EXT-X-ENDLIST") {
		t.Fatalf("initial video playlist advertises unmaterialized media:\n%s", video)
	}
	subtitles, err := os.ReadFile(dir + "/subs.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#EXT-X-MEDIA-SEQUENCE:0", "subs_000000.vtt?v=v1", "subs_000001.vtt?v=v1", "subs_000002.vtt?v=v1", "#EXT-X-ENDLIST"} {
		if !strings.Contains(string(subtitles), want) {
			t.Fatalf("virtual subtitle playlist missing %q:\n%s", want, subtitles)
		}
	}
}

func TestUsesTextSubtitlesRejectsBitmapTracks(t *testing.T) {
	selection := StreamSelection{SubtitleTrackIndex: 0}
	if UsesTextSubtitles(domain.MediaInfo{Subtitles: []domain.SubtitleTrack{{Codec: "hdmv_pgs_subtitle"}}}, selection) {
		t.Fatal("UsesTextSubtitles() = true for bitmap subtitles")
	}
	if !UsesTextSubtitles(domain.MediaInfo{Subtitles: []domain.SubtitleTrack{{Codec: "subrip"}}}, selection) {
		t.Fatal("UsesTextSubtitles() = false for text subtitles")
	}
}

func TestPrepareOnDemandHLSWritesProgressiveManifestWithoutDuration(t *testing.T) {
	dir := t.TempDir()
	err := PrepareOnDemandHLS(
		dir,
		dir+"/index.m3u8",
		dir+"/subs.m3u8",
		dir+"/master.m3u8",
		domain.MediaInfo{},
		StreamSelection{SubtitleTrackIndex: -1},
		6*time.Second,
		3,
		"v1",
	)
	if err != nil {
		t.Fatalf("PrepareOnDemandHLS() error = %v", err)
	}
	assertFileContains(t, dir+"/index.m3u8", "#EXT-X-MEDIA-SEQUENCE:0")
	assertFileContains(t, dir+"/index.m3u8", "#EXT-X-DISCONTINUITY-SEQUENCE:0")
	data, readErr := os.ReadFile(dir + "/index.m3u8")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(data), "#EXT-X-ENDLIST") {
		t.Fatalf("initial progressive playlist is final:\n%s", data)
	}
	if strings.Contains(string(data), "chunk_000000.ts") {
		t.Fatalf("initial progressive playlist must not advertise a missing segment:\n%s", data)
	}
}

func TestPrepareOnDemandHLSKeepsCompatibleUnknownDurationProgressive(t *testing.T) {
	dir := t.TempDir()
	info := domain.MediaInfo{VideoCodec: "hevc", VideoProfile: "Main 10", PixelFormat: "yuv420p10le"}
	if err := PrepareOnDemandHLS(
		dir, dir+"/index.m3u8", "", dir+"/master.m3u8",
		info, StreamSelection{SubtitleTrackIndex: -1},
		6*time.Second, 3, "v1",
	); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dir + "/index.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "seek_") || strings.Contains(string(data), "#EXT-X-ENDLIST") {
		t.Fatalf("unknown-duration remux received a sparse VOD timeline:\n%s", data)
	}
	assertFileContains(t, dir+"/master.m3u8", "#EXT-X-VERSION:7")
}

func TestBuildMaterializedPlaylistContainsOnlyMaterializedFragments(t *testing.T) {
	got := buildMaterializedPlaylist(36*time.Second, "v1", true, 4, 0, 27, false, []HLSFragment{
		{Start: 24, Duration: 5.5, Name: "direct_000004_0000.m4s", Init: "init_000004.mp4"},
		{Start: 29.5, Duration: 6.5, Name: "direct_000004_0001.m4s", Init: "init_000004.mp4"},
	})
	for _, want := range []string{
		"#EXT-X-TARGETDURATION:36",
		"#EXT-X-MEDIA-SEQUENCE:4",
		"#EXT-X-DISCONTINUITY-SEQUENCE:0",
		"#EXT-X-START:TIME-OFFSET=3.000,PRECISE=YES",
		"#EXT-X-MAP:URI=\"init_000004.mp4?v=v1\"",
		"direct_000004_0000.m4s?v=v1",
		"direct_000004_0001.m4s?v=v1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("progressive direct playlist missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"seek_", "chunk_", "#EXT-X-PLAYLIST-TYPE", "#EXT-X-ENDLIST"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("progressive direct playlist contains %q:\n%s", unwanted, got)
		}
	}
	if strings.Contains(got, "#EXT-X-INDEPENDENT-SEGMENTS") {
		t.Fatalf("split direct playlist claims independent segments:\n%s", got)
	}
}

func TestBuildMaterializedPlaylistOmitsStartAfterTargetLeavesTail(t *testing.T) {
	got := buildMaterializedPlaylist(12*time.Second, "v1", false, 8, 2, 27, true, []HLSFragment{
		{Start: 48, Duration: 6, Name: "chunk_000008.ts"},
	})
	if strings.Contains(got, "#EXT-X-START") {
		t.Fatalf("playlist retained a start outside its materialized tail:\n%s", got)
	}
	if !strings.Contains(got, "#EXT-X-ENDLIST") {
		t.Fatalf("final playlist lacks end marker:\n%s", got)
	}
	if !strings.Contains(got, "#EXT-X-DISCONTINUITY-SEQUENCE:2") {
		t.Fatalf("playlist lost its discontinuity sequence:\n%s", got)
	}
}

func TestDirectWindowGenerationDurationUsesAdmittedPrerollBudget(t *testing.T) {
	if got := DirectWindowGenerationDuration(5, 6*time.Second, 42*time.Second); got != 72*time.Second {
		t.Fatalf("generation duration = %s, want 1m12s", got)
	}
}

func TestProgressivePlaylistKeepsBoundedLiveTail(t *testing.T) {
	got := buildProgressiveMediaPlaylist(6, 3, "chunk_", ".ts", "v1", 8, 0, false)
	for _, want := range []string{
		"#EXT-X-MEDIA-SEQUENCE:5\n",
		"#EXT-X-DISCONTINUITY-SEQUENCE:1\n",
		"chunk_000005.ts?v=v1\n",
		"#EXT-X-DISCONTINUITY\n#EXTINF:6.000,\nchunk_000006.ts?v=v1\n",
		"chunk_000007.ts?v=v1\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("progressive playlist missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"#EXT-X-PLAYLIST-TYPE:EVENT", "chunk_000004.ts", "#EXT-X-ENDLIST"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("progressive playlist contains %q:\n%s", unwanted, got)
		}
	}
}

func TestAddWebVTTTimestampMapUsesAbsoluteMediaTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.vtt")
	if err := os.WriteFile(path, []byte("WEBVTT\n\n00:01.000 --> 00:03.000\ntext\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := addWebVTTTimestampMap(path, 6*time.Second); err != nil {
		t.Fatal(err)
	}
	assertFileContains(t, path, "X-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:540000")
}

func TestGenerateVideoWindowRejectsRangePastDuration(t *testing.T) {
	_, err := GenerateVideoWindow(
		context.Background(),
		"input", t.TempDir(),
		domain.MediaInfo{Duration: 10},
		StreamSelection{},
		2, 1, 10*time.Second, nil,
	)
	if err == nil {
		t.Fatal("GenerateVideoWindow() error = nil, want range error")
	}
}

func TestPublishVideoSegmentsPublishesOnlyRequestedRange(t *testing.T) {
	workDir := t.TempDir()
	outDir := t.TempDir()
	writeWindowPlaylist(t, workDir, 6, 6, 6, 6)
	for _, index := range []int{4, 5, 6, 7} {
		name := filepath.Join(workDir, fmt.Sprintf("chunk_%06d.ts", index))
		if err := os.WriteFile(name, []byte(fmt.Sprintf("segment-%d", index)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	close(done)
	count, err := publishVideoSegments(context.Background(), workDir, outDir, 4, 7, done, nil)
	if err != nil {
		t.Fatalf("publishVideoSegments() error = %v", err)
	}
	if count != 3 {
		t.Fatalf("publishVideoSegments() count = %d, want 3", count)
	}
	for _, index := range []int{4, 5, 6} {
		if _, err := os.Stat(filepath.Join(outDir, fmt.Sprintf("chunk_%06d.ts", index))); err != nil {
			t.Fatalf("requested segment %d was not published: %v", index, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outDir, "chunk_000007.ts")); !os.IsNotExist(err) {
		t.Fatalf("extra segment was published: %v", err)
	}
}

func TestPublishVideoSegmentsAllowsEndOfUnknownInput(t *testing.T) {
	workDir := t.TempDir()
	outDir := t.TempDir()
	writeWindowPlaylist(t, workDir, 6)
	if err := os.WriteFile(filepath.Join(workDir, "chunk_000000.ts"), []byte("segment"), 0644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	count, err := publishVideoSegments(context.Background(), workDir, outDir, 0, 4, done, nil)
	if err != nil {
		t.Fatalf("publishVideoSegments() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("publishVideoSegments() count = %d, want 1", count)
	}
}

func TestPublishVideoSegmentsDropsZeroDurationTail(t *testing.T) {
	workDir := t.TempDir()
	outDir := t.TempDir()
	writeWindowPlaylist(t, workDir, 6, 0)
	for index := 0; index < 2; index++ {
		if err := os.WriteFile(filepath.Join(workDir, fmt.Sprintf("chunk_%06d.ts", index)), []byte("segment"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	close(done)
	count, err := publishVideoSegments(context.Background(), workDir, outDir, 0, 2, done, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("published %d segments, want only the non-empty segment", count)
	}
	if _, err := os.Stat(filepath.Join(outDir, "chunk_000001.ts")); !os.IsNotExist(err) {
		t.Fatalf("zero-duration segment was published: %v", err)
	}
}

func writeWindowPlaylist(t *testing.T, workDir string, durations ...float64) {
	t.Helper()
	var playlist strings.Builder
	playlist.WriteString("#EXTM3U\n")
	for index, duration := range durations {
		fmt.Fprintf(&playlist, "#EXTINF:%.3f,\nchunk_%06d.ts\n", duration, index)
	}
	if err := os.WriteFile(filepath.Join(workDir, "window.m3u8"), []byte(playlist.String()), 0644); err != nil {
		t.Fatal(err)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s missing %q:\n%s", path, want, data)
	}
}
