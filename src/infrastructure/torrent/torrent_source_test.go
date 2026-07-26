package torrent

import (
	"context"
	"reflect"
	"testing"
	"time"

	anacrolix "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestPlaybackReadaheadIsPieceAlignedAndStageBounded(t *testing.T) {
	const maximum = int64(128 << 20)
	const piece = int64(4 << 20)
	if got := playbackReadaheadBytes(maximum, piece, 40_000_000, 4*time.Second, 16<<20); got != 16<<20 {
		t.Fatalf("startup readahead = %d, want 16 MiB", got)
	}
	if got := playbackReadaheadBytes(maximum, piece, 40_000_000, 15*time.Second, 0); got != 72<<20 {
		t.Fatalf("foreground readahead = %d, want 72 MiB", got)
	}
	if got := playbackReadaheadBytes(32<<20, piece, 40_000_000, 30*time.Second, 0); got != 32<<20 {
		t.Fatalf("bounded readahead = %d, want 32 MiB", got)
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

func TestAddMagnetSharesOneTorrentRuntimePerInfoHash(t *testing.T) {
	config := anacrolix.TestingConfig(t)
	client, err := anacrolix.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	first, err := addMagnet(client, magnet)
	if err != nil {
		t.Fatal(err)
	}
	second, err := addMagnet(client, magnet)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("same infohash created more than one torrent runtime")
	}
	if got := len(client.Torrents()); got != 1 {
		t.Fatalf("torrent runtimes = %d, want 1", got)
	}
}

func TestPieceDemandIsReferenceCountedAcrossPlaybackSessions(t *testing.T) {
	config := anacrolix.TestingConfig(t)
	client, err := anacrolix.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	pieceHash := metainfo.HashBytes([]byte{1, 2, 3, 4})
	info := metainfo.Info{Name: "movie", PieceLength: 4, Length: 4, Pieces: pieceHash[:]}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	torrentRuntime, _ := client.AddTorrentOpt(anacrolix.AddTorrentOpts{
		InfoBytes:                infoBytes,
		InfoHash:                 metainfo.HashBytes(infoBytes),
		DisableInitialPieceCheck: true,
	})
	demand := newPieceDemand()
	releaseFirst := demand.acquire(torrentRuntime, 0, 1)
	releaseSecond := demand.acquire(torrentRuntime, 0, 1)
	if err := torrentRuntime.Piece(0).VerifyDataContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state anacrolix.PieceState
	deadline := time.Now().Add(time.Second)
	for {
		state = torrentRuntime.Piece(0).State()
		if !state.Marking || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if state.Priority != anacrolix.PiecePriorityNow {
		t.Fatalf("active playback piece state = %+v, want priority %v", state, anacrolix.PiecePriorityNow)
	}
	key := pieceDemandKey{torrent: torrentRuntime, index: 0}
	refs := func() (int, bool) {
		demand.mu.Lock()
		defer demand.mu.Unlock()
		count, ok := demand.refs[key]
		return count, ok
	}
	if got, _ := refs(); got != 2 {
		t.Fatalf("shared piece demand refs = %d, want 2", got)
	}

	releaseFirst()
	if got, _ := refs(); got != 1 {
		t.Fatalf("piece demand after one session release = %d, want 1", got)
	}
	releaseSecond()
	if _, ok := refs(); ok {
		t.Fatal("piece demand remained after every playback session released it")
	}
	if got := torrentRuntime.Piece(0).State().Priority; got != anacrolix.PiecePriorityNone {
		t.Fatalf("released playback piece priority = %v, want %v", got, anacrolix.PiecePriorityNone)
	}
}
