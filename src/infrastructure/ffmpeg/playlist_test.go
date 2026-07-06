package ffmpeg

import "testing"

func TestBuildMasterPlaylistWithoutSubtitles(t *testing.T) {
	got := buildMasterPlaylist("index.m3u8", "subs.m3u8", false, "")
	want := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000\n" +
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
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000,SUBTITLES=\"subs\"\n" +
		"index.m3u8\n"

	if got != want {
		t.Fatalf("buildMasterPlaylist() = %q, want %q", got, want)
	}
}

func TestPlaylistHasSegment(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{
			name: "empty playlist",
			data: "#EXTM3U\n#EXT-X-TARGETDURATION:2\n",
		},
		{
			name: "master playlist",
			data: "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000\nindex.m3u8\n",
		},
		{
			name: "extinf without uri",
			data: "#EXTM3U\n#EXTINF:2.000,\n#EXT-X-ENDLIST\n",
		},
		{
			name: "media playlist segment",
			data: "#EXTM3U\n#EXTINF:2.000,\nchunk_00000.ts\n",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := playlistHasSegment(tt.data); got != tt.want {
				t.Fatalf("playlistHasSegment() = %v, want %v", got, tt.want)
			}
		})
	}
}
