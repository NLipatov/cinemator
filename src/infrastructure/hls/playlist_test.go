package hls

import "testing"

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
