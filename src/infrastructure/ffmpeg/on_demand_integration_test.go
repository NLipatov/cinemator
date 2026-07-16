package ffmpeg

import (
	"cinemator/domain"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGenerateVideoWindowUsesAbsoluteTimelineAndMappedWebVTT(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	runMediaCommand(t, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25:duration=14",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=14",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", source,
	)

	outDir := filepath.Join(dir, "hls")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatal(err)
	}
	result, err := GenerateVideoWindow(
		context.Background(), source, outDir,
		domain.MediaInfo{
			Duration:    14,
			VideoCodec:  "h264",
			Width:       160,
			Height:      90,
			AudioTracks: []domain.AudioTrack{{Codec: "aac"}},
		},
		StreamSelection{AudioTrackIndex: 0},
		1, 2, 6*time.Second, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Generated != 2 {
		t.Fatalf("generated segments = %d, want 2", result.Generated)
	}
	probe := runMediaCommand(t, "ffprobe",
		"-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=pts_time", "-of", "default=nw=1:nk=1",
		"-read_intervals", "%+0.05", filepath.Join(outDir, "chunk_000001.ts"),
	)
	firstPTS, err := strconv.ParseFloat(strings.TrimSpace(strings.Split(string(probe), "\n")[0]), 64)
	if err != nil {
		t.Fatalf("parse first PTS %q: %v", probe, err)
	}
	if firstPTS < 5.9 || firstPTS > 6.2 {
		t.Fatalf("first PTS = %.3f, want absolute timestamp near 6s", firstPTS)
	}
	progressiveDir := filepath.Join(dir, "progressive")
	if err := os.MkdirAll(progressiveDir, 0755); err != nil {
		t.Fatal(err)
	}
	var published []int
	progressive, err := GenerateVideoWindow(
		context.Background(), source, progressiveDir,
		domain.MediaInfo{VideoCodec: "h264", Width: 160, Height: 90, AudioTracks: []domain.AudioTrack{{Codec: "aac"}}},
		StreamSelection{AudioTrackIndex: 0},
		0, 5, 6*time.Second,
		func(index int) { published = append(published, index) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !progressive.ReachedEnd || progressive.Generated != 3 || len(published) != 3 {
		t.Fatalf("progressive result = %+v, published=%v", progressive, published)
	}
	exactDir := filepath.Join(dir, "exact-boundary")
	if err := os.MkdirAll(exactDir, 0755); err != nil {
		t.Fatal(err)
	}
	firstExact, err := GenerateVideoWindow(
		context.Background(), source, exactDir,
		domain.MediaInfo{VideoCodec: "h264", Width: 160, Height: 90, AudioTracks: []domain.AudioTrack{{Codec: "aac"}}},
		StreamSelection{AudioTrackIndex: 0},
		0, 2, 7*time.Second, nil,
	)
	if err != nil || firstExact.Generated != 2 {
		t.Fatalf("exact-boundary first window = %+v, err=%v", firstExact, err)
	}
	afterExact, err := GenerateVideoWindow(
		context.Background(), source, exactDir,
		domain.MediaInfo{VideoCodec: "h264", Width: 160, Height: 90, AudioTracks: []domain.AudioTrack{{Codec: "aac"}}},
		StreamSelection{AudioTrackIndex: 0},
		2, 2, 7*time.Second, nil,
	)
	if err != nil || !afterExact.ReachedEnd || afterExact.Generated != 0 {
		t.Fatalf("exact-boundary EOF window = %+v, err=%v", afterExact, err)
	}
	overstatedDir := filepath.Join(dir, "overstated")
	if err := os.MkdirAll(overstatedDir, 0755); err != nil {
		t.Fatal(err)
	}
	overstated, err := GenerateVideoWindow(
		context.Background(), source, overstatedDir,
		domain.MediaInfo{Duration: 30, VideoCodec: "h264", Width: 160, Height: 90, AudioTracks: []domain.AudioTrack{{Codec: "aac"}}},
		StreamSelection{AudioTrackIndex: 0},
		2, 3, 6*time.Second, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !overstated.ReachedEnd || overstated.Generated != 1 {
		t.Fatalf("overstated-duration result = %+v", overstated)
	}

	srt := filepath.Join(dir, "subs.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:07,000 --> 00:00:09,000\nsecond window\n"), 0644); err != nil {
		t.Fatal(err)
	}
	withSubs := filepath.Join(dir, "source-with-subs.mkv")
	runMediaCommand(t, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", source, "-i", srt, "-map", "0", "-map", "1", "-c", "copy", "-c:s", "srt", withSubs)
	vtt := filepath.Join(dir, "subs_000001.vtt")
	if err := GenerateSubtitleSegment(context.Background(), withSubs, vtt, 0, 1, 6*time.Second); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(vtt)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "X-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:540000") || !strings.Contains(text, "00:01.") || !strings.Contains(text, "second window") {
		t.Fatalf("unexpected WebVTT segment:\n%s", text)
	}
}

func runMediaCommand(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", name, err, out)
	}
	return out
}
