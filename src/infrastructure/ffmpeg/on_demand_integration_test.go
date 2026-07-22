package ffmpeg

import (
	"bytes"
	"cinemator/domain"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGenerateEmptySubtitleSegmentDoesNotReadUntilTheNextCue(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}

	dir := t.TempDir()
	srt := filepath.Join(dir, "sparse.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:18,000 --> 00:00:19,000\nlate cue\n"), 0644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "sparse.mkv")
	runMediaCommand(t, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=96x54:rate=5:duration=20",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000:duration=20",
		"-i", srt,
		"-map", "0:v:0", "-map", "1:a:0", "-map", "2:s:0",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "10", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-c:s", "srt", source,
	)
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	server := newPrefixOnlyRangeServer(data, len(data)*3/4)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output := filepath.Join(dir, "subs_000000.vtt")
	if err := GenerateSubtitleSegment(ctx, server.URL, output, 0, 0, 2*time.Second); err != nil {
		t.Fatalf("generate empty subtitle segment without reading to the 18s cue: %v", err)
	}
	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "X-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:0") || strings.Contains(string(contents), "late cue") {
		t.Fatalf("unexpected empty WebVTT segment:\n%s", contents)
	}
}

func newPrefixOnlyRangeServer(data []byte, prefixEnd int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		start := 0
		rangeHeader := request.Header.Get("Range")
		if value := rangeHeader; strings.HasPrefix(value, "bytes=") {
			rangeStart := strings.TrimSuffix(strings.TrimPrefix(value, "bytes="), "-")
			if parsed, err := strconv.Atoi(rangeStart); err == nil {
				start = parsed
			}
		}
		if start >= len(data)-4*1024 {
			http.ServeContent(response, request, "sparse.mkv", time.Time{}, bytes.NewReader(data))
			return
		}

		response.Header().Set("Accept-Ranges", "bytes")
		response.Header().Set("Content-Type", "video/x-matroska")
		if rangeHeader != "" {
			response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
			response.Header().Set("Content-Length", strconv.Itoa(len(data)-start))
			response.WriteHeader(http.StatusPartialContent)
		} else {
			response.Header().Set("Content-Length", strconv.Itoa(len(data)))
		}
		if start < prefixEnd {
			_, _ = response.Write(data[start:prefixEnd])
		}
		<-request.Context().Done()
	}))
}

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
	tailDuration, err := (SampleAnalyzer{}).AnalyzeTailDurationURL(context.Background(), source, 0)
	if err != nil {
		t.Fatalf("analyze tail duration: %v", err)
	}
	if tailDuration < 13.8 || tailDuration > 14.2 {
		t.Fatalf("tail duration = %.3f, want about 14s", tailDuration)
	}

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
	directDir := filepath.Join(dir, "direct")
	if err := os.MkdirAll(directDir, 0755); err != nil {
		t.Fatal(err)
	}
	var publishedDirect []HLSFragment
	direct, err := GenerateDirectWindow(
		context.Background(), source, directDir,
		domain.MediaInfo{
			Duration:    14,
			VideoCodec:  "h264",
			Width:       160,
			Height:      90,
			AudioTracks: []domain.AudioTrack{{Codec: "aac"}},
		},
		StreamSelection{AudioTrackIndex: 0},
		1, 1, 6*time.Second, 30*time.Second,
		func(fragment HLSFragment) error {
			if _, err := os.Stat(filepath.Join(directDir, fragment.Name)); err != nil {
				return err
			}
			publishedDirect = append(publishedDirect, fragment)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(direct.Fragments) == 0 || direct.Fragments[0].Start >= 6 {
		t.Fatalf("direct window = %+v, want preroll before 6s", direct)
	}
	if len(publishedDirect) != len(direct.Fragments) {
		t.Fatalf("published direct fragments = %d, result = %d", len(publishedDirect), len(direct.Fragments))
	}
	directProbe := runMediaCommand(t, "ffprobe",
		"-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name", "-of", "default=nw=1:nk=1",
		filepath.Join(directDir, direct.Fragments[0].Name),
	)
	if !strings.Contains(string(directProbe), "h264") {
		t.Fatalf("direct codec = %q, want h264", directProbe)
	}
	unknownDirectDir := filepath.Join(dir, "unknown-direct")
	if err := os.MkdirAll(unknownDirectDir, 0755); err != nil {
		t.Fatal(err)
	}
	unknownDirect, err := GenerateDirectWindow(
		context.Background(), source, unknownDirectDir,
		domain.MediaInfo{
			VideoCodec: "h264", PixelFormat: "yuv420p",
			AudioTracks: []domain.AudioTrack{{Codec: "aac"}},
		},
		StreamSelection{AudioTrackIndex: 0},
		0, 5, 6*time.Second, 30*time.Second, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !unknownDirect.ReachedEnd || len(unknownDirect.Fragments) == 0 {
		t.Fatalf("unknown-duration direct result = %+v", unknownDirect)
	}
	hybridSource := filepath.Join(dir, "source-ac3.mkv")
	runMediaCommand(t, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y", "-i", source,
		"-f", "lavfi", "-i", "anullsrc=r=48000:cl=5.1(side):d=14",
		"-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy", "-c:a", "ac3", "-shortest", hybridSource,
	)
	hybridDir := filepath.Join(dir, "hybrid")
	if err := os.MkdirAll(hybridDir, 0755); err != nil {
		t.Fatal(err)
	}
	hybrid, err := GenerateDirectWindow(
		context.Background(), hybridSource, hybridDir,
		domain.MediaInfo{
			Duration:    14,
			VideoCodec:  "h264",
			Width:       160,
			Height:      90,
			AudioTracks: []domain.AudioTrack{{Codec: "ac3", Channels: 6}},
		},
		StreamSelection{AudioTrackIndex: 0},
		0, 2, 6*time.Second, 30*time.Second, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(hybrid.Fragments) < 2 {
		t.Fatalf("hybrid fragments = %+v, want at least two", hybrid.Fragments)
	}
	hybridAudio := runMediaCommand(t, "ffprobe",
		"-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name,sample_rate,channels", "-of", "default=nw=1:nk=1",
		filepath.Join(hybridDir, hybrid.Fragments[1].Name),
	)
	for _, want := range []string{"aac", "48000", "6"} {
		if !strings.Contains(string(hybridAudio), want) {
			t.Fatalf("second hybrid segment audio = %q, want %s", hybridAudio, want)
		}
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
		func(index int, _ float64) error {
			published = append(published, index)
			return nil
		},
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
	if err := os.WriteFile(srt, []byte("1\n00:00:05,000 --> 00:00:08,000\ncrosses boundary\n\n2\n00:00:06,000 --> 00:00:06,500\nstarts at boundary\n\n3\n00:00:07,000 --> 00:00:09,000\nsecond window\n"), 0644); err != nil {
		t.Fatal(err)
	}
	withSubs := filepath.Join(dir, "source-with-subs.mkv")
	runMediaCommand(t, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", source, "-i", srt, "-map", "0", "-map", "1", "-c", "copy", "-c:s", "srt", withSubs)
	firstVTT := filepath.Join(dir, "subs_000000.vtt")
	if err := GenerateSubtitleSegment(context.Background(), withSubs, firstVTT, 0, 0, 6*time.Second); err != nil {
		t.Fatal(err)
	}
	firstData, err := os.ReadFile(firstVTT)
	if err != nil {
		t.Fatal(err)
	}
	secondVTT := filepath.Join(dir, "subs_000001.vtt")
	if err := GenerateSubtitleSegment(context.Background(), withSubs, secondVTT, 0, 1, 6*time.Second); err != nil {
		t.Fatal(err)
	}
	secondData, err := os.ReadFile(secondVTT)
	if err != nil {
		t.Fatal(err)
	}
	firstText := string(firstData)
	secondText := string(secondData)
	if !strings.Contains(firstText, "X-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:0") || !strings.Contains(firstText, "crosses boundary") {
		t.Fatalf("unexpected first WebVTT segment:\n%s", firstText)
	}
	if !strings.Contains(secondText, "X-TIMESTAMP-MAP=LOCAL:00:00:06.000,MPEGTS:540000") || !strings.Contains(secondText, "crosses boundary") || !strings.Contains(secondText, "starts at boundary") || !strings.Contains(secondText, "second window") {
		t.Fatalf("unexpected second WebVTT segment:\n%s\nfirst segment:\n%s", secondText, firstText)
	}
	firstTiming := webVTTCueTiming(firstText, "crosses boundary")
	secondTiming := webVTTCueTiming(secondText, "crosses boundary")
	if firstTiming == "" || secondTiming != firstTiming {
		t.Fatalf("cross-boundary cue timing changed between segments: %q != %q\n%s\n%s", firstTiming, secondTiming, firstText, secondText)
	}
	if got := strings.Count(firstText+secondText, "starts at boundary"); got != 1 {
		t.Fatalf("boundary cue was published %d times, want once:\n%s\n%s", got, firstText, secondText)
	}
}

func webVTTCueTiming(contents, cueText string) string {
	lines := strings.Split(contents, "\n")
	for index, line := range lines {
		if strings.TrimSpace(line) == cueText && index > 0 {
			return strings.TrimSpace(lines[index-1])
		}
	}
	return ""
}

func TestGenerateDirectWindowCoalescesFrequentKeyframes(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "all-intra.mp4")
	runMediaCommand(t, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25:duration=4",
		"-c:v", "libx264", "-g", "1", "-pix_fmt", "yuv420p", source,
	)

	result, err := GenerateDirectWindow(
		context.Background(), source, dir,
		domain.MediaInfo{Duration: 4, VideoCodec: "h264", Width: 160, Height: 90},
		StreamSelection{SubtitleTrackIndex: -1},
		0, 2, 2*time.Second, 30*time.Second, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Fragments) > 3 {
		t.Fatalf("direct window exposed %d fragments for 100 keyframes: %+v", len(result.Fragments), result)
	}
}

func TestGenerateDirectWindowPreservesFMP4Video(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	encoders := string(runMediaCommand(t, "ffmpeg", "-hide_banner", "-encoders"))
	tests := []struct {
		name, encoder, codec, tag, profile string
		level                              int
		encoderArgs                        []string
	}{
		{"HEVC Main 10", "libx265", "hevc", "hvc1", "Main 10", 30, []string{"-preset", "ultrafast", "-x265-params", "log-level=error:keyint=50:min-keyint=50:scenecut=0:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc"}},
		{"AV1 Main 10", "libsvtav1", "av1", "av01", "Main", 0, []string{"-preset", "11", "-g", "50", "-svtav1-params", "color-primaries=9:transfer-characteristics=16:matrix-coefficients=9"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(encoders, tt.encoder) {
				t.Skip(tt.encoder + " is not installed")
			}
			dir := t.TempDir()
			source := filepath.Join(dir, "source.mkv")
			args := []string{
				"-hide_banner", "-loglevel", "error", "-y",
				"-f", "lavfi", "-i", "testsrc2=size=160x96:rate=25:duration=4",
				"-c:v", tt.encoder,
			}
			args = append(args, tt.encoderArgs...)
			args = append(args,
				"-pix_fmt", "yuv420p10le",
				"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc",
				source,
			)
			runMediaCommand(t, "ffmpeg", args...)

			result, err := GenerateDirectWindow(
				context.Background(), source, dir,
				domain.MediaInfo{
					Duration:     4,
					VideoCodec:   tt.codec,
					VideoProfile: tt.profile,
					VideoLevel:   tt.level,
					PixelFormat:  "yuv420p10le",
					HDR:          true,
				},
				StreamSelection{SubtitleTrackIndex: -1},
				0, 1, 2*time.Second, 30*time.Second, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Fragments) == 0 || result.Fragments[0].Init == "" || !strings.HasSuffix(result.Fragments[0].Name, ".m4s") {
				t.Fatalf("direct fMP4 result = %+v", result)
			}
			probeURL := "concat:" + filepath.Join(dir, result.Fragments[0].Init) + "|" + filepath.Join(dir, result.Fragments[0].Name)
			probe := runMediaCommand(t, "ffprobe",
				"-v", "error", "-select_streams", "v:0",
				"-show_entries", "stream=codec_name,codec_tag_string,pix_fmt,color_primaries,color_transfer,color_space",
				"-of", "default=nw=1", probeURL,
			)
			for _, want := range []string{
				"codec_name=" + tt.codec, "codec_tag_string=" + tt.tag, "pix_fmt=yuv420p10le",
				"color_primaries=bt2020", "color_transfer=smpte2084", "color_space=bt2020nc",
			} {
				if !strings.Contains(string(probe), want) {
					t.Fatalf("fMP4 probe missing %q: %s", want, probe)
				}
			}
		})
	}
}

func TestAnalyzeTailDurationBeyondSeekWindow(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "long-source.mp4")
	runMediaCommand(t, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=1:duration=40",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "1", "-pix_fmt", "yuv420p", source,
	)

	duration, err := (SampleAnalyzer{}).AnalyzeTailDurationURL(context.Background(), source, 0)
	if err != nil {
		t.Fatalf("analyze tail duration: %v", err)
	}
	if duration < 39.9 || duration > 40.1 {
		t.Fatalf("tail duration = %.3f, want about 40s", duration)
	}
	outDir := filepath.Join(dir, "bounded-direct")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatal(err)
	}
	window, err := GenerateDirectWindow(
		context.Background(), source, outDir,
		domain.MediaInfo{Duration: 40, VideoCodec: "h264", Width: 64, Height: 64},
		StreamSelection{SubtitleTrackIndex: -1},
		2, 1, 6*time.Second, 30*time.Second, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	last := window.Fragments[len(window.Fragments)-1]
	if end := last.Start + last.Duration; end > 18.1 {
		t.Fatalf("direct remux read through %.3fs, want it stopped at the first closing keyframe", end)
	}

	longGOP := filepath.Join(dir, "long-gop.mp4")
	runMediaCommand(t, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y", "-i", source,
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "100", "-keyint_min", "100", "-sc_threshold", "0",
		"-pix_fmt", "yuv420p", longGOP,
	)
	tail, err := GenerateDirectWindow(
		context.Background(), longGOP, t.TempDir(),
		domain.MediaInfo{Duration: 40, VideoCodec: "h264", Width: 64, Height: 64},
		StreamSelection{SubtitleTrackIndex: -1},
		5, 1, 6*time.Second, 40*time.Second, nil,
	)
	if err != nil {
		t.Fatalf("final long-GOP window fell back to transcoding: %v", err)
	}
	if !tail.ReachedEnd || len(tail.Fragments) == 0 {
		t.Fatalf("final long-GOP window = %+v, want a finalized direct tail", tail)
	}
	for _, fragment := range tail.Fragments {
		if fragment.Duration > 6.25 {
			t.Fatalf("long-GOP fragment duration = %.3fs, want at most the configured 6s boundary", fragment.Duration)
		}
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
