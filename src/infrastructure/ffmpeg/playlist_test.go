package ffmpeg

import (
	"cinemator/domain"
	"strings"
	"testing"
	"time"
)

func TestBuildMasterPlaylistWithoutSubtitles(t *testing.T) {
	got := buildMasterPlaylist("index.m3u8", "subs.m3u8", false, "", "", domain.MediaInfo{}, StreamSelection{}, 6*time.Second)
	want := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=9166667\n" +
		"index.m3u8\n"

	if got != want {
		t.Fatalf("buildMasterPlaylist() = %q, want %q", got, want)
	}
}

func TestBuildMasterPlaylistWithSubtitles(t *testing.T) {
	got := buildMasterPlaylist("index.m3u8", "subs.m3u8", true, "eng", "", domain.MediaInfo{}, StreamSelection{}, 6*time.Second)
	want := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"Subtitles\",DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,URI=\"subs.m3u8\",LANGUAGE=\"eng\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=9166667,SUBTITLES=\"subs\"\n" +
		"index.m3u8\n"

	if got != want {
		t.Fatalf("buildMasterPlaylist() = %q, want %q", got, want)
	}
}

func TestBuildMasterPlaylistSignalsFMP4HDRCodec(t *testing.T) {
	info := domain.MediaInfo{
		Duration:         60,
		VideoCodec:       "hevc",
		VideoCodecString: "hvc1.2.4.L153.B0",
		VideoProfile:     "Main 10",
		VideoLevel:       153,
		PixelFormat:      "yuv420p10le",
		HDR:              true,
		HDRFormat:        "HDR10",
		Bitrate:          20_000_000,
		AudioTracks:      []domain.AudioTrack{{Codec: "aac"}},
	}
	got := buildMasterPlaylist("index.m3u8", "", false, "", "", info, StreamSelection{}, 6*time.Second)
	for _, want := range []string{
		"#EXT-X-VERSION:7\n",
		"BANDWIDTH=20000000",
		"CODECS=\"hvc1.2.4.L153.B0,mp4a.40.2\"",
		"VIDEO-RANGE=PQ",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("buildMasterPlaylist() missing %q: %s", want, got)
		}
	}
}

func TestBuildMasterPlaylistOmitsUnknownCodecString(t *testing.T) {
	got := buildMasterPlaylist(
		"index.m3u8", "", false, "", "v1",
		domain.MediaInfo{Duration: 60, VideoCodec: "hevc", VideoProfile: "Main 10", VideoLevel: 153, PixelFormat: "yuv420p10le"},
		StreamSelection{}, 6*time.Second,
	)
	if strings.Contains(got, "CODECS=") {
		t.Fatalf("master playlist synthesized an unknown codec configuration:\n%s", got)
	}
}

func TestBuildMasterPlaylistDropsSourceCodecForFallback(t *testing.T) {
	info := domain.MediaInfo{
		Duration:         60,
		VideoCodec:       "hevc",
		VideoCodecString: "hvc1.2.4.L153.B0",
		VideoProfile:     "Main 10",
		VideoLevel:       153,
		PixelFormat:      "yuv420p10le",
		HDR:              true,
		HDRFormat:        "HDR10",
	}
	got := buildMasterPlaylist("index.m3u8", "", false, "", "", info, StreamSelection{ForceTranscode: true}, 6*time.Second)
	if !strings.Contains(got, "#EXT-X-VERSION:3") || strings.Contains(got, "hvc1") || strings.Contains(got, "VIDEO-RANGE") {
		t.Fatalf("fallback master advertises source capabilities: %s", got)
	}
}

func TestBuildMasterPlaylistAdvertisesCompatibilityPeak(t *testing.T) {
	info := domain.MediaInfo{
		Duration:    60,
		VideoCodec:  "hevc",
		Width:       3840,
		Height:      2160,
		FrameRate:   30,
		Bitrate:     10_000_000,
		AudioTracks: []domain.AudioTrack{{Codec: "aac"}},
	}
	got := buildMasterPlaylist("index.m3u8", "", false, "", "", info, StreamSelection{ForceTranscode: true}, 6*time.Second)
	if !strings.Contains(got, "BANDWIDTH=52000000") {
		t.Fatalf("buildMasterPlaylist() does not advertise compatibility peak: %s", got)
	}
}
