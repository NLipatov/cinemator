package ffmpeg

import (
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
	if len(segments) != 3 {
		t.Fatalf("len(segments) = %d, want 3", len(segments))
	}

	assertDuration(t, segments[0].Duration, 52.678)
	assertDuration(t, segments[1].Duration, 220.762)
	assertDuration(t, segments[2].Duration, 26.560)
	if !strings.Contains(got, "#EXT-X-TARGETDURATION:221\n") {
		t.Fatalf("normalized playlist target duration does not cover the largest gap:\n%s", got)
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
