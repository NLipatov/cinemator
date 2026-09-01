package torrent

import (
	"errors"
	"testing"
)

const (
	v1Magnet     = "magnet:?xt=urn:btih:631a31dd0a46257d5078c0dee4e66e26f73e42ac"
	v2Magnet     = "magnet:?xt=urn:btmh:1220caf1e1c30e81cb361b9ee167c4aa64228a7fa4fa9f6105232b28ad099f3a302e"
	hybridMagnet = v1Magnet +
		"&xt=urn:btmh:1220d8dd32ac93357c368556af3ac1d95c9d76bd0dff6fa9833ecdac3d53134efabb"
)

func TestParseMagnetRejectsV2Only(t *testing.T) {
	if _, _, err := parseMagnet(v2Magnet); !errors.Is(err, ErrUnsupportedMagnetVersion) {
		t.Fatalf("parseMagnet() error = %v, want ErrUnsupportedMagnetVersion", err)
	}
}

func TestParseMagnetAcceptsV1AndHybrid(t *testing.T) {
	const wantHash = "631a31dd0a46257d5078c0dee4e66e26f73e42ac"
	for _, magnet := range []string{v1Magnet, hybridMagnet} {
		_, gotHash, err := parseMagnet(magnet)
		if err != nil {
			t.Fatalf("parseMagnet() error = %v", err)
		}
		if gotHash != wantHash {
			t.Fatalf("parseMagnet() hash = %q, want %q", gotHash, wantHash)
		}
	}
}

func TestClampRange(t *testing.T) {
	tests := []struct {
		name       string
		offset     int64
		length     int64
		fileLength int64
		wantOffset int64
		wantLength int64
	}{
		{name: "inside", offset: 10, length: 20, fileLength: 100, wantOffset: 10, wantLength: 20},
		{name: "negative offset", offset: -5, length: 20, fileLength: 100, wantOffset: 0, wantLength: 15},
		{name: "past end", offset: 80, length: 50, fileLength: 100, wantOffset: 80, wantLength: 20},
		{name: "starts after file", offset: 120, length: 10, fileLength: 100, wantOffset: 120, wantLength: 0},
		{name: "empty file", offset: 0, length: 10, fileLength: 0, wantOffset: 0, wantLength: 0},
		{name: "negative range", offset: -50, length: 20, fileLength: 100, wantOffset: 0, wantLength: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotOffset, gotLength := clampRange(tt.offset, tt.length, tt.fileLength)
			if gotOffset != tt.wantOffset || gotLength != tt.wantLength {
				t.Fatalf("clampRange() = %d, %d; want %d, %d", gotOffset, gotLength, tt.wantOffset, tt.wantLength)
			}
		})
	}
}
