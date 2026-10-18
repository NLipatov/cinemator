package media

import "testing"

func TestPlaylistDuration(t *testing.T) {
	data := "#EXTM3U\n#EXTINF:2.5,\nfirst.ts\n#EXTINF:3,\nsecond.ts\n#EXTINF:9,\n"
	if got, err := PlaylistDuration([]byte(data)); err != nil || got != 5.5 {
		t.Fatalf("PlaylistDuration = %g, %v; want 5.5", got, err)
	}
	for _, value := range []string{"broken", "NaN", "+Inf", "-1"} {
		if got, err := PlaylistDuration([]byte("#EXTM3U\n#EXTINF:" + value + ",\nvideo.ts\n")); err == nil || got != 0 {
			t.Fatalf("duration %q: got %g, %v; want an error", value, got, err)
		}
	}
}

func TestHasSegment(t *testing.T) {
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
			if got := HasSegment(tt.data); got != tt.want {
				t.Fatalf("HasSegment() = %v, want %v", got, tt.want)
			}
		})
	}
}
