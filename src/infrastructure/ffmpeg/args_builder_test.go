package ffmpeg

import (
	"cinemator/domain"
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
	for _, want := range []string{"-filter_complex", "[0:v]format=yuv420p[base];[base][0:s:0]overlay,format=yuv420p[v]", "[v]", "0:a:0"} {
		if !slices.Contains(args, want) {
			t.Fatalf("buildStreamArgs() missing %q: %v", want, args)
		}
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

func TestBuildStreamArgsBoundsResolutionAndBitrate(t *testing.T) {
	args := buildStreamArgs(domain.MediaInfo{VideoCodec: "hevc", Width: 3840, Height: 2160}, StreamSelection{})
	joined := strings.Join(args, " ")
	for _, want := range []string{"scale=1920:1080", "-b:v 4000k", "-maxrate 5000k", "format=yuv420p"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("buildStreamArgs() missing %q: %v", want, args)
		}
	}
}
