package ffmpeg

import (
	"cinemator/domain"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildOnDemandMediaPlaylistCoversWholeDuration(t *testing.T) {
	got := buildOnDemandMediaPlaylist(25.5, 10, "chunk_", ".ts")

	for _, want := range []string{
		"#EXT-X-PLAYLIST-TYPE:VOD\n",
		"#EXTINF:10.000,\nchunk_000000.ts\n",
		"#EXTINF:10.000,\nchunk_000001.ts\n",
		"#EXTINF:5.500,\nchunk_000002.ts\n",
		"#EXT-X-ENDLIST\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("playlist missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "#EXT-X-DISCONTINUITY") {
		t.Fatalf("absolute-timestamp segments must not force decoder discontinuities:\n%s", got)
	}
}

func TestPrepareOnDemandHLSWritesStaticManifests(t *testing.T) {
	dir := t.TempDir()
	info := domain.MediaInfo{
		Duration:  30,
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
	)
	if err != nil {
		t.Fatalf("PrepareOnDemandHLS() error = %v", err)
	}
	assertFileContains(t, dir+"/master.m3u8", "SUBTITLES=\"subs\"")
	assertFileContains(t, dir+"/index.m3u8", "chunk_000002.ts")
	assertFileContains(t, dir+"/subs.m3u8", "subs_000002.vtt")
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
	)
	if err != nil {
		t.Fatalf("PrepareOnDemandHLS() error = %v", err)
	}
	assertFileContains(t, dir+"/index.m3u8", "#EXT-X-PLAYLIST-TYPE:EVENT")
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

func TestUpdateProgressiveHLSFinalizesDiscoveredDuration(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/index.m3u8"
	if err := UpdateProgressiveHLS(path, "", false, 6*time.Second, 3, 2.5, true); err != nil {
		t.Fatalf("UpdateProgressiveHLS() error = %v", err)
	}
	assertFileContains(t, path, "#EXTINF:2.500,\nchunk_000002.ts")
	assertFileContains(t, path, "#EXT-X-ENDLIST")
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
