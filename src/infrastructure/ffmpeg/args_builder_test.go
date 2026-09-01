package ffmpeg

import (
	"cinemator/domain"
	"path/filepath"
	"slices"
	"testing"
)

func TestArgsBuilderUsesURLInput(t *testing.T) {
	args := ArgsBuilder{
		OutDir: t.TempDir(),
		Input:  "http://127.0.0.1/source/token",
	}.BuildShared(domain.MediaInfo{VideoCodec: "h264"})

	if !slices.Contains(args, "http://127.0.0.1/source/token") {
		t.Fatalf("BuildShared() args do not include URL input: %v", args)
	}
	if slices.Contains(args, "pipe:0") {
		t.Fatalf("BuildShared() args unexpectedly include pipe input: %v", args)
	}
}

func TestArgsBuilderCreatesVideoAndEveryAudioRendition(t *testing.T) {
	dir := t.TempDir()
	args := ArgsBuilder{OutDir: dir, Input: "input"}.BuildShared(domain.MediaInfo{
		VideoCodec: "h264",
		AudioTracks: []domain.AudioTrack{
			{Codec: "aac"},
			{Codec: "ac3"},
		},
	})

	for _, want := range []string{
		"0:v:0",
		"0:a:0",
		"0:a:1",
		filepath.Join(dir, "index.m3u8"),
		filepath.Join(dir, "audio_0.m3u8"),
		filepath.Join(dir, "audio_1.m3u8"),
		filepath.Join(dir, "video_%05d.ts"),
		filepath.Join(dir, "audio_0_%05d.ts"),
		filepath.Join(dir, "audio_1_%05d.ts"),
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("BuildShared() args do not include %q: %v", want, args)
		}
	}
	if got := countArg(args, "-map"); got != 3 {
		t.Fatalf("BuildShared() map count = %d, want 3: %v", got, args)
	}
}

func TestArgsBuilderCopiesAACAndEncodesOtherAudio(t *testing.T) {
	args := ArgsBuilder{OutDir: t.TempDir(), Input: "input"}.BuildShared(domain.MediaInfo{
		VideoCodec: "h264",
		AudioTracks: []domain.AudioTrack{
			{Codec: "aac"},
			{Codec: "ac3"},
		},
	})

	if got := countArg(args, "-c:a"); got != 2 {
		t.Fatalf("BuildShared() audio codec count = %d, want 2: %v", got, args)
	}
	if !slices.Contains(args, "128k") {
		t.Fatalf("BuildShared() does not encode non-AAC audio: %v", args)
	}
}

func TestArgsBuilderAllowsVideoOnlyInput(t *testing.T) {
	args := ArgsBuilder{OutDir: t.TempDir(), Input: "input"}.
		BuildShared(domain.MediaInfo{VideoCodec: "h264"})

	if slices.Contains(args, "-c:a") {
		t.Fatalf("BuildShared() unexpectedly configures missing audio: %v", args)
	}
	if got := countArg(args, "-map"); got != 1 {
		t.Fatalf("BuildShared() map count = %d, want 1: %v", got, args)
	}
}

func TestArgsBuilderPublishesAppendOnlyEventPlaylistsWithoutMuxDelay(t *testing.T) {
	args := ArgsBuilder{OutDir: t.TempDir(), Input: "input"}.
		BuildShared(domain.MediaInfo{VideoCodec: "h264"})

	playlistType := slices.Index(args, "-hls_playlist_type")
	if playlistType < 0 || playlistType+1 >= len(args) || args[playlistType+1] != "event" {
		t.Fatalf("BuildShared() does not declare an event playlist: %v", args)
	}
	muxDelay := slices.Index(args, "-muxdelay")
	if muxDelay < 0 || muxDelay+1 >= len(args) || args[muxDelay+1] != "0" {
		t.Fatalf("BuildShared() does not disable MPEG-TS mux delay: %v", args)
	}
}

func TestArgsBuilderUsesVideoOnlyOverlayForBitmapSubtitles(t *testing.T) {
	args := ArgsBuilder{OutDir: t.TempDir(), Input: "input"}.BuildBitmap(domain.MediaInfo{
		VideoCodec: "h264",
		AudioTracks: []domain.AudioTrack{
			{Codec: "aac"},
		},
		Subtitles: []domain.SubtitleTrack{
			{Codec: "hdmv_pgs_subtitle"},
		},
	}, 0)

	if !slices.Contains(args, "[0:v:0][0:s:0]overlay[v]") {
		t.Fatalf("BuildBitmap() does not overlay the selected subtitle: %v", args)
	}
	if slices.Contains(args, "0:a:0") || slices.Contains(args, "-c:a") {
		t.Fatalf("BuildBitmap() unexpectedly duplicates shared audio: %v", args)
	}
}

func countArg(args []string, want string) int {
	count := 0
	for _, arg := range args {
		if arg == want {
			count++
		}
	}
	return count
}
