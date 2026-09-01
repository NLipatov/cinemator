package media

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildNormalizedSubtitlePlaylistKeepsCueTimelineGaps(t *testing.T) {
	dir := t.TempDir()
	writeTestVTT(t, dir, "subs_00000.vtt", "00:44.628", "00:47.089")
	writeTestVTT(t, dir, "subs_00001.vtt", "00:52.678", "00:54.304")
	writeTestVTT(t, dir, "subs_00002.vtt", "04:33.440", "04:38.362")

	rawSegments := []playlistSegment{
		{URI: "subs_00000.vtt", Duration: 47.089},
		{URI: "subs_00001.vtt", Duration: 1.626},
		{URI: "subs_00002.vtt", Duration: 4.922},
	}

	got, ok, complete, err := buildNormalizedSubtitlePlaylist(rawSegments, true, 300, true, dir)
	if err != nil {
		t.Fatalf("buildNormalizedSubtitlePlaylist() error = %v", err)
	}
	if !ok {
		t.Fatal("buildNormalizedSubtitlePlaylist() reported no segments")
	}
	if !complete {
		t.Fatal("buildNormalizedSubtitlePlaylist() reported incomplete inputs")
	}

	segments, ended, err := parseMediaPlaylist([]byte(got))
	if err != nil {
		t.Fatalf("parseMediaPlaylist() error = %v", err)
	}
	if !ended {
		t.Fatal("normalized playlist does not include ENDLIST")
	}
	if len(segments) != 15 {
		t.Fatalf("len(segments) = %d, want 15", len(segments))
	}

	for i := 0; i < 11; i++ {
		if segments[i].URI != subtitlePrerollFilename {
			t.Fatalf("segments[%d].URI = %q, want %q", i, segments[i].URI, subtitlePrerollFilename)
		}
		assertDuration(t, segments[i].Duration, 4)
	}
	assertDuration(t, segments[11].Duration, 0.628)
	for i, want := range []string{"subs_00000.vtt", "subs_00001.vtt", "subs_00002.vtt"} {
		if segments[12+i].URI != want {
			t.Fatalf("segments[%d].URI = %q, want %q", 12+i, segments[12+i].URI, want)
		}
	}
	assertDuration(t, segments[12].Duration, 8.050)
	assertDuration(t, segments[13].Duration, 220.762)
	assertDuration(t, segments[14].Duration, 26.560)
	if !strings.Contains(got, "#EXT-X-TARGETDURATION:221\n") {
		t.Fatalf("normalized playlist target duration does not cover the largest gap:\n%s", got)
	}
}

func TestBuildNormalizedSubtitlePlaylistPublishesPrerollBeforeFirstCue(t *testing.T) {
	got, ok, complete, err := buildNormalizedSubtitlePlaylist(nil, false, 9.5, false, t.TempDir())
	if err != nil {
		t.Fatalf("buildNormalizedSubtitlePlaylist() error = %v", err)
	}
	if !ok {
		t.Fatal("buildNormalizedSubtitlePlaylist() reported no segments")
	}
	if !complete {
		t.Fatal("buildNormalizedSubtitlePlaylist() reported incomplete inputs")
	}

	segments, ended, err := parseMediaPlaylist([]byte(got))
	if err != nil {
		t.Fatalf("parseMediaPlaylist() error = %v", err)
	}
	if ended {
		t.Fatal("preroll playlist unexpectedly includes ENDLIST")
	}
	if len(segments) != 3 {
		t.Fatalf("len(segments) = %d, want 3", len(segments))
	}
	for i, want := range []float64{4, 4, 1.5} {
		if segments[i].URI != subtitlePrerollFilename {
			t.Fatalf("segments[%d].URI = %q, want %q", i, segments[i].URI, subtitlePrerollFilename)
		}
		assertDuration(t, segments[i].Duration, want)
	}
	if !strings.Contains(got, "#EXT-X-TARGETDURATION:4\n") {
		t.Fatalf("preroll playlist target duration is not bounded:\n%s", got)
	}
}

func TestBuildNormalizedSubtitlePlaylistDoesNotEndWhileSegmentIsMissing(t *testing.T) {
	dir := t.TempDir()
	writeTestVTT(t, dir, "subs_00000.vtt", "00:01.000", "00:02.000")

	tests := []struct {
		name     string
		segments []playlistSegment
	}{
		{
			name:     "first segment",
			segments: []playlistSegment{{URI: "missing.vtt", Duration: 1}},
		},
		{
			name: "later segment",
			segments: []playlistSegment{
				{URI: "subs_00000.vtt", Duration: 2},
				{URI: "missing.vtt", Duration: 1},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			playlist, ok, complete, err := buildNormalizedSubtitlePlaylist(tt.segments, true, 10, true, dir)
			if err != nil {
				t.Fatalf("buildNormalizedSubtitlePlaylist() error = %v", err)
			}
			if !ok {
				t.Fatal("buildNormalizedSubtitlePlaylist() reported no segments")
			}
			if complete {
				t.Fatal("buildNormalizedSubtitlePlaylist() reported complete inputs")
			}
			if strings.Contains(playlist, "#EXT-X-ENDLIST") {
				t.Fatalf("incomplete subtitle playlist includes ENDLIST:\n%s", playlist)
			}
		})
	}
}

func TestAppendSubtitlePrerollAvoidsSubMillisecondTail(t *testing.T) {
	segments := appendSubtitlePreroll(nil, 8.0001)
	if len(segments) != 2 {
		t.Fatalf("len(segments) = %d, want 2", len(segments))
	}
	assertDuration(t, segments[0].Duration, 4)
	assertDuration(t, segments[1].Duration, 4)
}

func TestAppendSubtitlePrerollDoesNotInventTimeAtZero(t *testing.T) {
	segments := appendSubtitlePreroll(nil, 0)
	if len(segments) != 0 {
		t.Fatalf("len(segments) = %d, want 0", len(segments))
	}
}

func TestSubtitlePlaylistNormalizerPublishesPrerollBeforeRawPlaylist(t *testing.T) {
	dir := t.TempDir()
	videoList := filepath.Join(dir, "index.m3u8")
	outPlaylist := filepath.Join(dir, "subs.m3u8")
	if err := os.WriteFile(videoList, []byte("#EXTM3U\n#EXTINF:3.5,\nchunk_00000.ts\n"), 0644); err != nil {
		t.Fatal(err)
	}

	normalizer := subtitlePlaylistNormalizer{
		ctx:         context.Background(),
		rawPlaylist: filepath.Join(dir, "missing.raw.m3u8"),
		outPlaylist: outPlaylist,
		videoList:   videoList,
		segmentDir:  dir,
	}
	done, err := normalizer.refreshOnce()
	if err != nil {
		t.Fatalf("refreshOnce() error = %v", err)
	}
	if done {
		t.Fatal("refreshOnce() completed before subtitle extraction")
	}
	data, err := os.ReadFile(outPlaylist)
	if err != nil {
		t.Fatalf("read preroll playlist: %v", err)
	}
	if !strings.Contains(string(data), subtitlePrerollFilename) {
		t.Fatalf("preroll playlist does not reference %s:\n%s", subtitlePrerollFilename, data)
	}
}

func TestSubtitlePrerollMakesRenditionReadyBeforeFirstCue(t *testing.T) {
	dir := t.TempDir()
	videoList := filepath.Join(dir, "index.m3u8")
	subtitleList := filepath.Join(dir, "subs_0.m3u8")
	master := filepath.Join(dir, "master.m3u8")
	if err := os.WriteFile(videoList, []byte("#EXTM3U\n#EXTINF:3.5,\nchunk_00000.ts\n"), 0644); err != nil {
		t.Fatal(err)
	}

	normalizer := subtitlePlaylistNormalizer{
		ctx:         context.Background(),
		rawPlaylist: filepath.Join(dir, "missing.raw.m3u8"),
		outPlaylist: subtitleList,
		videoList:   videoList,
		segmentDir:  dir,
	}
	if _, err := normalizer.refreshOnce(); err != nil {
		t.Fatalf("refreshOnce() error = %v", err)
	}

	converter := Converter{
		ctx:       context.Background(),
		info:      MediaInfo{Subtitles: []SubtitleTrack{{Codec: "subrip"}}},
		builder:   argsBuilder{OutDir: dir},
		videoList: videoList,
		master:    master,
	}
	if err := converter.writeMasterAfterRenditionsReady(); err != nil {
		t.Fatalf("writeMasterAfterRenditionsReady() error = %v", err)
	}
	data, err := os.ReadFile(master)
	if err != nil {
		t.Fatalf("read master playlist: %v", err)
	}
	if !strings.Contains(string(data), "index.m3u8") {
		t.Fatalf("readiness master does not include video rendition:\n%s", data)
	}
}

func TestConverterUsesTrackSpecificSubtitlePaths(t *testing.T) {
	dir := t.TempDir()
	converter := Converter{
		builder:  argsBuilder{OutDir: dir},
		inputURL: "input",
	}
	args := converter.subtitleArgs(2, filepath.Join(dir, "subs_2.raw.m3u8"))
	joined := strings.Join(args, " ")
	for _, want := range []string{"0:s:2", "subs_2.raw.m3u8", "subs_2_%05d.vtt"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("subtitleArgs() does not contain %q: %v", want, args)
		}
	}
}

func TestBuildNormalizedSubtitlePlaylistFillsEmptyTrack(t *testing.T) {
	dir := t.TempDir()

	playlist, ok, complete, err := buildNormalizedSubtitlePlaylist(nil, true, 300, true, dir)
	if err != nil {
		t.Fatalf("buildNormalizedSubtitlePlaylist() error = %v", err)
	}
	if !ok {
		t.Fatal("buildNormalizedSubtitlePlaylist() reported no filler segments")
	}
	if !complete {
		t.Fatal("buildNormalizedSubtitlePlaylist() reported incomplete inputs")
	}
	if !strings.Contains(playlist, subtitlePrerollFilename) || !strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Fatalf("empty subtitle track is not a complete filler playlist:\n%s", playlist)
	}
}

func TestBuildNormalizedSubtitlePlaylistFillsCueLessSegments(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "subs_00000.vtt"), []byte("WEBVTT\n\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rawSegments := []playlistSegment{{URI: "subs_00000.vtt", Duration: 1}}
	playlist, ok, complete, err := buildNormalizedSubtitlePlaylist(rawSegments, true, 300, true, dir)
	if err != nil {
		t.Fatalf("buildNormalizedSubtitlePlaylist() error = %v", err)
	}
	if !ok {
		t.Fatal("buildNormalizedSubtitlePlaylist() reported no filler segments")
	}
	if !complete {
		t.Fatal("buildNormalizedSubtitlePlaylist() reported incomplete inputs")
	}
	if !strings.Contains(playlist, subtitlePrerollFilename) || !strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Fatalf("cue-less subtitle track is not a complete filler playlist:\n%s", playlist)
	}
}

func TestSubtitlePlaylistNormalizerWaitsForVideoAfterEmptySubtitleCompletes(t *testing.T) {
	done := make(chan struct{})
	close(done)
	normalizer := subtitlePlaylistNormalizer{
		ctx:               context.Background(),
		rawPlaylist:       filepath.Join(t.TempDir(), "missing.m3u8"),
		subtitleCompleted: done,
	}

	completed, err := normalizer.refreshOnce()
	if err != nil {
		t.Fatalf("refreshOnce() error = %v", err)
	}
	if completed {
		t.Fatal("refreshOnce() completed before the video timeline was available")
	}
}

func TestParseWebVTTTime(t *testing.T) {
	tests := []struct {
		raw  string
		want float64
	}{
		{raw: "00:44.628", want: 44.628},
		{raw: "04:33.440", want: 273.440},
		{raw: "01:02:03.456", want: 3723.456},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := parseWebVTTTime(tt.raw)
			if err != nil {
				t.Fatalf("parseWebVTTTime() error = %v", err)
			}
			assertDuration(t, got, tt.want)
		})
	}
}

func writeTestVTT(t *testing.T, dir, name, start, end string) {
	t.Helper()
	data := "WEBVTT\n\n" + start + " --> " + end + "\nText\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func assertDuration(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.001 {
		t.Fatalf("duration = %.3f, want %.3f", got, want)
	}
}
