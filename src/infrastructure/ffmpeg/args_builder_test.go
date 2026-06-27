package ffmpeg

import (
	"cinemator/domain"
	"slices"
	"testing"
)

func TestArgsBuilderUsesURLInput(t *testing.T) {
	args := ArgsBuilder{
		OutDir:   t.TempDir(),
		Playlist: "index.m3u8",
		Input:    "http://127.0.0.1/source/token",
	}.Build(domain.MediaInfo{
		VideoCodec: "h264",
		AudioTracks: []domain.AudioTrack{
			{Codec: "aac"},
		},
	}, StreamSelection{})

	if !slices.Contains(args, "http://127.0.0.1/source/token") {
		t.Fatalf("Build() args do not include URL input: %v", args)
	}
	if slices.Contains(args, "pipe:0") {
		t.Fatalf("Build() args unexpectedly include pipe input: %v", args)
	}
}

func TestArgsBuilderAllowsVideoOnlyInput(t *testing.T) {
	args := ArgsBuilder{
		OutDir:   t.TempDir(),
		Playlist: "index.m3u8",
		Input:    "http://127.0.0.1/source/token",
	}.Build(domain.MediaInfo{VideoCodec: "h264"}, StreamSelection{})

	if slices.Contains(args, "0:a:0") {
		t.Fatalf("Build() args unexpectedly map missing audio: %v", args)
	}
	if slices.Contains(args, "-c:a") {
		t.Fatalf("Build() args unexpectedly configure missing audio: %v", args)
	}
}

func TestArgsBuilderPreservesAudioMapWithTextSubtitles(t *testing.T) {
	args := ArgsBuilder{
		OutDir:   t.TempDir(),
		Playlist: "index.m3u8",
		Input:    "http://127.0.0.1/source/token",
	}.Build(domain.MediaInfo{
		VideoCodec: "h264",
		AudioTracks: []domain.AudioTrack{
			{Codec: "aac"},
			{Codec: "ac3"},
		},
		Subtitles: []domain.SubtitleTrack{
			{Codec: "subrip"},
		},
	}, StreamSelection{AudioTrackIndex: 1, SubtitleTrackIndex: 0})

	if !slices.Contains(args, "0:a:1") {
		t.Fatalf("Build() args do not preserve selected audio map: %v", args)
	}
	if slices.Contains(args, "-filter_complex") {
		t.Fatalf("Build() args unexpectedly use bitmap subtitle filter for text subtitles: %v", args)
	}
}

func TestArgsBuilderUsesOverlayForBitmapSubtitles(t *testing.T) {
	args := ArgsBuilder{
		OutDir:   t.TempDir(),
		Playlist: "index.m3u8",
		Input:    "http://127.0.0.1/source/token",
	}.Build(domain.MediaInfo{
		VideoCodec: "h264",
		AudioTracks: []domain.AudioTrack{
			{Codec: "aac"},
		},
		Subtitles: []domain.SubtitleTrack{
			{Codec: "hdmv_pgs_subtitle"},
		},
	}, StreamSelection{SubtitleTrackIndex: 0})

	if !slices.Contains(args, "-filter_complex") {
		t.Fatalf("Build() args do not use overlay path for bitmap subtitles: %v", args)
	}
	if !slices.Contains(args, "[0:v][0:s:0]overlay[v]") {
		t.Fatalf("Build() args do not overlay selected bitmap subtitle: %v", args)
	}
	if !slices.Contains(args, "[v]") {
		t.Fatalf("Build() args do not map filtered video: %v", args)
	}
	if !slices.Contains(args, "0:a:0") {
		t.Fatalf("Build() args do not preserve audio map with bitmap subtitles: %v", args)
	}
}
