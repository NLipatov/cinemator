package torrent

import "testing"

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
