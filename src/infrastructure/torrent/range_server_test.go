package torrent

import "testing"

func TestParseByteRange(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		size    int64
		start   int64
		end     int64
		partial bool
	}{
		{name: "full", size: 100, start: 0, end: 99},
		{name: "open ended", header: "bytes=10-", size: 100, start: 10, end: 99, partial: true},
		{name: "closed", header: "bytes=10-19", size: 100, start: 10, end: 19, partial: true},
		{name: "clamped", header: "bytes=90-999", size: 100, start: 90, end: 99, partial: true},
		{name: "suffix", header: "bytes=-20", size: 100, start: 80, end: 99, partial: true},
		{name: "large suffix", header: "bytes=-200", size: 100, start: 0, end: 99, partial: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, partial, err := parseByteRange(tt.header, tt.size)
			if err != nil {
				t.Fatalf("parseByteRange() error = %v", err)
			}
			if start != tt.start || end != tt.end || partial != tt.partial {
				t.Fatalf("parseByteRange() = %d, %d, %t; want %d, %d, %t", start, end, partial, tt.start, tt.end, tt.partial)
			}
		})
	}
}

func TestParseByteRangeRejectsInvalidRanges(t *testing.T) {
	tests := []string{
		"items=0-1",
		"bytes=200-201",
		"bytes=10-1",
		"bytes=-0",
		"bytes=0-1,2-3",
		"bytes=x-y",
	}

	for _, header := range tests {
		t.Run(header, func(t *testing.T) {
			if _, _, _, err := parseByteRange(header, 100); err == nil {
				t.Fatalf("parseByteRange(%q) succeeded, want error", header)
			}
		})
	}
}
