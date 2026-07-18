package ffmpeg

import (
	"cinemator/domain"
	"math"
	"slices"
	"strings"
	"testing"
)

func TestBuildStreamArgsTranscodesH264AndAACForExactSegmentBoundaries(t *testing.T) {
	args := buildStreamArgs(domain.MediaInfo{
		VideoCodec:  "h264",
		AudioTracks: []domain.AudioTrack{{Codec: "aac"}},
	}, StreamSelection{AudioTrackIndex: 0})
	joined := strings.Join(args, " ")
	for _, want := range []string{"-c:v libx264", "-c:a aac"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("buildStreamArgs() missing %q: %v", want, args)
		}
	}
	if strings.Contains(joined, " copy") {
		t.Fatalf("buildStreamArgs() uses stream copy: %v", args)
	}
}

func TestBuildStreamArgsAllowsVideoOnlyInput(t *testing.T) {
	args := buildStreamArgs(domain.MediaInfo{VideoCodec: "h264"}, StreamSelection{})
	if slices.Contains(args, "0:a:0") || slices.Contains(args, "-c:a") {
		t.Fatalf("buildStreamArgs() configures missing audio: %v", args)
	}
}

func TestBuildStreamArgsPreservesAudioMapWithTextSubtitles(t *testing.T) {
	args := buildStreamArgs(domain.MediaInfo{
		VideoCodec:  "h264",
		AudioTracks: []domain.AudioTrack{{Codec: "aac"}, {Codec: "ac3"}},
		Subtitles:   []domain.SubtitleTrack{{Codec: "subrip"}},
	}, StreamSelection{AudioTrackIndex: 1, SubtitleTrackIndex: 0})
	if !slices.Contains(args, "0:a:1") || slices.Contains(args, "-filter_complex") {
		t.Fatalf("buildStreamArgs() mishandles text subtitles: %v", args)
	}
}

func TestBuildStreamArgsUsesOverlayForBitmapSubtitles(t *testing.T) {
	args := buildStreamArgs(domain.MediaInfo{
		VideoCodec:  "h264",
		AudioTracks: []domain.AudioTrack{{Codec: "aac"}},
		Subtitles:   []domain.SubtitleTrack{{Codec: "hdmv_pgs_subtitle"}},
	}, StreamSelection{SubtitleTrackIndex: 0})
	for _, want := range []string{"-filter_complex", "[0:v:0]format=yuv420p[base];[base][0:s:0]overlay,format=yuv420p[v]", "[v]", "0:a:0"} {
		if !slices.Contains(args, want) {
			t.Fatalf("buildStreamArgs() missing %q: %v", want, args)
		}
	}
}

func TestBuildStreamArgsMapsVideoAfterAttachedCoverArt(t *testing.T) {
	args := buildStreamArgs(domain.MediaInfo{VideoCodec: "h264", VideoTrackIndex: 1}, StreamSelection{})
	if !slices.Contains(args, "0:v:1") {
		t.Fatalf("buildStreamArgs() did not map selected video: %v", args)
	}
}

func TestBuildStreamArgsToneMapsHDRBeforeBitmapSubtitles(t *testing.T) {
	args := buildStreamArgs(domain.MediaInfo{
		VideoCodec: "hevc",
		HDR:        true,
		Subtitles:  []domain.SubtitleTrack{{Codec: "hdmv_pgs_subtitle"}},
	}, StreamSelection{SubtitleTrackIndex: 0})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"zscale=t=linear:npl=100",
		"tonemap=tonemap=hable:desat=0",
		"zscale=t=bt709:m=bt709:r=tv",
		"[base];[base][0:s:0]overlay",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("buildStreamArgs() missing %q: %v", want, args)
		}
	}
	if strings.Contains(joined, "-vf") {
		t.Fatalf("buildStreamArgs() mixes -vf with bitmap filter graph: %v", args)
	}
}

func TestBuildStreamArgsPreservesResolutionAndBoundsPeakRate(t *testing.T) {
	args := buildStreamArgs(domain.MediaInfo{VideoCodec: "hevc", Width: 3840, Height: 2160}, StreamSelection{})
	joined := strings.Join(args, " ")
	for _, want := range []string{"-crf 18", "format=yuv420p", "-maxrate 31104000", "-bufsize 62208000"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("buildStreamArgs() missing %q: %v", want, args)
		}
	}
	for _, unwanted := range []string{"scale=", "-b:v"} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("buildStreamArgs() contains %q: %v", unwanted, args)
		}
	}
}

func TestHLSReservationBitrateKeepsDirectVideoUntouched(t *testing.T) {
	info := domain.MediaInfo{VideoCodec: "av1", VideoProfile: "Main", PixelFormat: "yuv420p10le", Duration: 60, Width: 7680, Height: 4320, FrameRate: 60, Bitrate: 80_000_000}
	if !CanRemuxHLS(info, StreamSelection{}) {
		t.Fatal("8K source unexpectedly requires transcoding")
	}
	if got := HLSReservationBitrate(info, StreamSelection{}); got != 160_000_000 {
		t.Fatalf("HLSReservationBitrate() = %d, want source peak 160000000", got)
	}
	if got := HLSReservationBitrate(info, StreamSelection{ForceTranscode: true}); got != 248_832_000 {
		t.Fatalf("forced HLSReservationBitrate() = %d, want compatibility peak 248832000", got)
	}
	args := strings.Join(buildRemuxStreamArgs(info, StreamSelection{}), " ")
	if strings.Contains(args, "maxrate") || !strings.Contains(args, "-c:v copy") {
		t.Fatalf("direct args alter source video: %s", args)
	}
}

func TestHLSReservationBitrateSaturates(t *testing.T) {
	info := domain.MediaInfo{Width: 1, Height: 1, FrameRate: math.MaxFloat64}
	if got := HLSReservationBitrate(info, StreamSelection{}); got != math.MaxInt64 {
		t.Fatalf("HLSReservationBitrate() = %d, want saturation at %d", got, int64(math.MaxInt64))
	}
}

func TestBuildStreamArgsDeinterlacesWithoutScaling(t *testing.T) {
	args := buildStreamArgs(domain.MediaInfo{VideoCodec: "h264", Width: 3840, Height: 2160, Deinterlace: true}, StreamSelection{})
	joined := strings.Join(args, " ")
	want := "bwdif=mode=send_frame:parity=auto:deint=interlaced,format=yuv420p"
	if !strings.Contains(joined, want) {
		t.Fatalf("buildStreamArgs() missing ordered filters %q: %v", want, args)
	}
	if strings.Contains(joined, "scale=") {
		t.Fatalf("buildStreamArgs() scales the source: %v", args)
	}
}

func TestHLSModePrefersVideoCopy(t *testing.T) {
	info := domain.MediaInfo{
		Duration:    60,
		VideoCodec:  "h264",
		Width:       1920,
		Height:      1080,
		AudioTracks: []domain.AudioTrack{{Codec: "aac"}, {Codec: "ac3"}},
	}
	if got := HLSMode(info, StreamSelection{AudioTrackIndex: 0}); got != "direct" {
		t.Fatalf("HLSMode(AAC) = %q, want direct", got)
	}
	if got := HLSMode(info, StreamSelection{AudioTrackIndex: 1}); got != "hybrid" {
		t.Fatalf("HLSMode(AC3) = %q, want hybrid", got)
	}
	multichannel := info
	multichannel.AudioTracks = []domain.AudioTrack{{Codec: "aac", Profile: "LC", Channels: 6, SampleRate: 48000}}
	if got := HLSMode(multichannel, StreamSelection{}); got != "hybrid" {
		t.Fatalf("HLSMode(multichannel AAC) = %q, want hybrid", got)
	}

	args := strings.Join(buildRemuxStreamArgs(info, StreamSelection{AudioTrackIndex: 1}), " ")
	if !strings.Contains(args, "-c:v copy") || !strings.Contains(args, "-c:a aac") {
		t.Fatalf("buildRemuxStreamArgs() = %q, want copied video and AAC audio", args)
	}
}

func TestCanRemuxHLSRejectsVideoTransformations(t *testing.T) {
	base := domain.MediaInfo{Duration: 60, VideoCodec: "h264", Width: 1280, Height: 720}
	tests := []struct {
		name string
		info domain.MediaInfo
	}{
		{name: "unknown duration", info: domain.MediaInfo{VideoCodec: "h264"}},
		{name: "codec", info: domain.MediaInfo{Duration: 60, VideoCodec: "vp9"}},
		{name: "profile", info: domain.MediaInfo{Duration: 60, VideoCodec: "h264", VideoProfile: "High 10", Width: 1280, Height: 720}},
		{name: "level", info: domain.MediaInfo{Duration: 60, VideoCodec: "h264", VideoProfile: "High", VideoLevel: 63, Width: 1280, Height: 720}},
		{name: "pixel format", info: func() domain.MediaInfo { v := base; v.NeedFilter = true; return v }()},
		{name: "HDR", info: func() domain.MediaInfo { v := base; v.HDR = true; return v }()},
		{name: "interlace", info: func() domain.MediaInfo { v := base; v.Deinterlace = true; return v }()},
		{name: "rotation", info: func() domain.MediaInfo { v := base; v.Rotated = true; return v }()},
		{name: "Dolby Vision", info: domain.MediaInfo{Duration: 60, VideoCodec: "hevc", VideoProfile: "Main 10", PixelFormat: "yuv420p10le", DolbyVision: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if CanRemuxHLS(tt.info, StreamSelection{}) {
				t.Fatal("CanRemuxHLS() = true")
			}
		})
	}

	text := base
	text.Subtitles = []domain.SubtitleTrack{{Codec: "subrip"}}
	if !CanRemuxHLS(text, StreamSelection{SubtitleTrackIndex: 0}) {
		t.Fatal("CanRemuxHLS() rejected separate text subtitles")
	}
	bitmap := base
	bitmap.Subtitles = []domain.SubtitleTrack{{Codec: "hdmv_pgs_subtitle"}}
	if CanRemuxHLS(bitmap, StreamSelection{SubtitleTrackIndex: 0}) {
		t.Fatal("CanRemuxHLS() accepted bitmap subtitle overlay")
	}
	if CanRemuxHLS(base, StreamSelection{ForceTranscode: true}) {
		t.Fatal("CanRemuxHLS() ignored forced native-HLS fallback")
	}
}

func TestCanRemuxHLSUsesFMP4ForHEVCAndAV1(t *testing.T) {
	for _, info := range []domain.MediaInfo{
		{Duration: 60, VideoCodec: "hevc", VideoProfile: "Main 10", VideoLevel: 153, PixelFormat: "yuv420p10le", HDR: true},
		{Duration: 60, VideoCodec: "av1", VideoProfile: "Main", VideoLevel: 18, PixelFormat: "yuv420p10le", HDR: true},
	} {
		if !CanRemuxHLS(info, StreamSelection{}) {
			t.Errorf("CanRemuxHLS(%s) = false", info.VideoCodec)
		}
		if !UsesFMP4(info, StreamSelection{}) {
			t.Errorf("UsesFMP4(%s) = false", info.VideoCodec)
		}
	}
}

func TestCanRemuxHLSPreserves4KAnd8KVideo(t *testing.T) {
	for _, info := range []domain.MediaInfo{
		{Duration: 60, VideoCodec: "h264", VideoProfile: "High", VideoLevel: 52, Width: 3840, Height: 2160},
		{Duration: 60, VideoCodec: "h264", VideoProfile: "High", VideoLevel: 62, Width: 7680, Height: 4320},
	} {
		if got := HLSMode(info, StreamSelection{}); got != "direct" {
			t.Errorf("HLSMode(%dx%d, level %d) = %q, want direct", info.Width, info.Height, info.VideoLevel, got)
		}
	}
}
