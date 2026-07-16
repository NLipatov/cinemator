package ffmpeg

import "testing"

func TestBuildMasterPlaylistWithoutSubtitles(t *testing.T) {
	got := buildMasterPlaylist("index.m3u8", "subs.m3u8", false, "")
	want := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=5500000\n" +
		"index.m3u8\n"

	if got != want {
		t.Fatalf("buildMasterPlaylist() = %q, want %q", got, want)
	}
}

func TestBuildMasterPlaylistWithSubtitles(t *testing.T) {
	got := buildMasterPlaylist("index.m3u8", "subs.m3u8", true, "eng")
	want := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"Subtitles\",DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,URI=\"subs.m3u8\",LANGUAGE=\"eng\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=5500000,SUBTITLES=\"subs\"\n" +
		"index.m3u8\n"

	if got != want {
		t.Fatalf("buildMasterPlaylist() = %q, want %q", got, want)
	}
}
