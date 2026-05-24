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
