package torrent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// This exercises the capped-storage failure that used to recurse in
// anacrolix/torrent.reader.readAt until the goroutine stack overflowed. The
// storage deliberately reports a verified piece and then fails every read,
// which is what an eviction race looks like to the torrent reader.
func TestForkReaderBoundsCappedStorageRetriesUntilCancellation(t *testing.T) {
	backend := &alwaysMissingCappedStorage{}
	cfg := atorrent.TestingConfig(t)
	cfg.DefaultStorage = backend
	cfg.Slogger = slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := atorrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	info := metainfo.Info{Name: "movie", PieceLength: 4, Length: 4, Pieces: make([]byte, 20)}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	tor, _ := client.AddTorrentOpt(atorrent.AddTorrentOpts{
		InfoBytes:                infoBytes,
		InfoHash:                 metainfo.HashBytes(infoBytes),
		DisableInitialPieceCheck: true,
		DisallowDataDownload:     true,
	})
	tor.Piece(0).UpdateCompletion()
	if got := tor.BytesCompleted(); got != info.TotalLength() {
		t.Fatalf("completed bytes = %d, want %d", got, info.TotalLength())
	}

	reader := tor.NewReader()
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = reader.ReadContext(ctx, make([]byte, 1))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("read returned after %v instead of waiting for capped storage", elapsed)
	}
	if reads := backend.reads.Load(); reads < 3 || reads > 30 {
		t.Fatalf("storage reads = %d, want bounded paced retries", reads)
	}
}

type alwaysMissingCappedStorage struct {
	reads atomic.Int64
}

func (s *alwaysMissingCappedStorage) OpenTorrent(context.Context, *metainfo.Info, metainfo.Hash) (storage.TorrentImpl, error) {
	capacity := func() (int64, bool) { return 4, true }
	return storage.TorrentImpl{
		Piece: func(metainfo.Piece) storage.PieceImpl {
			return alwaysMissingPiece{}
		},
		Capacity: &capacity,
		NewReader: func() storage.TorrentReader {
			return alwaysMissingTorrentReader{reads: &s.reads}
		},
	}, nil
}

type alwaysMissingPiece struct{}

func (alwaysMissingPiece) ReadAt([]byte, int64) (int, error)      { return 0, io.ErrUnexpectedEOF }
func (alwaysMissingPiece) WriteAt(p []byte, _ int64) (int, error) { return len(p), nil }
func (alwaysMissingPiece) MarkComplete() error                    { return nil }
func (alwaysMissingPiece) MarkNotComplete() error                 { return nil }
func (alwaysMissingPiece) Completion() storage.Completion {
	return storage.Completion{Ok: true, Complete: true}
}

type alwaysMissingTorrentReader struct {
	reads *atomic.Int64
}

func (r alwaysMissingTorrentReader) ReadAt([]byte, int64) (int, error) {
	r.reads.Add(1)
	return 0, io.ErrUnexpectedEOF
}

func (alwaysMissingTorrentReader) Close() error { return nil }
