package torrent

import (
	"reflect"
	"testing"

	anacrolix "github.com/anacrolix/torrent"
)

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

func TestSupportedTrackerTiers(t *testing.T) {
	tiers := [][]string{
		{"http://tracker.example/announce", "unsupported://tracker.example/announce"},
		{"udp://tracker.example:80", "udp4://tracker.example:80", "udp6://tracker.example:80"},
		{"ws://tracker.example", "wss://tracker.example"},
		{"not a tracker", "://invalid"},
	}
	want := [][]string{
		{"http://tracker.example/announce"},
		{"udp://tracker.example:80", "udp4://tracker.example:80", "udp6://tracker.example:80"},
		{"ws://tracker.example", "wss://tracker.example"},
	}

	if got := supportedTrackerTiers(tiers); !reflect.DeepEqual(got, want) {
		t.Fatalf("supportedTrackerTiers() = %#v; want %#v", got, want)
	}
}

func TestAddMagnetIgnoresUnsupportedTrackerSchemes(t *testing.T) {
	config := anacrolix.TestingConfig(t)
	config.DisableTrackers = false
	client, err := anacrolix.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	torrent, err := addMagnet(client, "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&tr=unsupported%3A%2F%2Ftracker.example%2Fannounce")
	if err != nil {
		t.Fatal(err)
	}
	if torrent == nil {
		t.Fatal("torrent was not created")
	}
}
