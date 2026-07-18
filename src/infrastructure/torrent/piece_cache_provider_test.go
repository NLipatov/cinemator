package torrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/missinggo/v2/filecache"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestPieceCacheTrimDoesNotUnlinkOpenPiece(t *testing.T) {
	root := t.TempDir()
	cache, err := filecache.NewCache(root)
	if err != nil {
		t.Fatal(err)
	}
	provider := newPieceCacheProvider(cache, root, 4, nil)
	first, _ := provider.NewInstance("completed/first")
	if err := first.(*pieceCacheInstance).PutSized(bytes.NewReader([]byte("aaaa")), 4); err != nil {
		t.Fatal(err)
	}
	reader, err := first.Get()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := provider.NewInstance("completed/second")
	if err := second.(*pieceCacheInstance).PutSized(bytes.NewReader([]byte("bbbb")), 4); err == nil {
		t.Fatal("write succeeded by evicting an open piece")
	}
	if _, err := first.Stat(); err != nil {
		t.Fatalf("open piece was unlinked: %v", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "aaaa" {
		t.Fatalf("read open piece = %q, %v", data, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.(*pieceCacheInstance).PutSized(bytes.NewReader([]byte("bbbb")), 4); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Stat(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released LRU piece remains: %v", err)
	}
}

func TestPieceChunkReaderPinsEveryChunk(t *testing.T) {
	root := t.TempDir()
	cache, err := filecache.NewCache(root)
	if err != nil {
		t.Fatal(err)
	}
	provider := newPieceCacheProvider(cache, root, 4, nil)
	for name, data := range map[string]string{"incompleted/hash/0": "aa", "incompleted/hash/2": "bb"} {
		instance, _ := provider.NewInstance(name)
		if err := instance.(*pieceCacheInstance).PutSized(bytes.NewReader([]byte(data)), int64(len(data))); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := provider.ChunksReader("incompleted/hash")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := provider.NewInstance("incompleted/other/0")
	if err := other.(*pieceCacheInstance).PutSized(bytes.NewReader([]byte("cccc")), 4); err == nil {
		t.Fatal("write succeeded by evicting chunks held for verification")
	}
	data := make([]byte, 4)
	if _, err := reader.ReadAt(data, 0); err != nil || string(data) != "aabb" {
		t.Fatalf("chunk read = %q, %v", data, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := other.(*pieceCacheInstance).PutSized(bytes.NewReader([]byte("cccc")), 4); err != nil {
		t.Fatal(err)
	}
}

func TestPieceDeleteWaitsForLastReader(t *testing.T) {
	root := t.TempDir()
	cache, err := filecache.NewCache(root)
	if err != nil {
		t.Fatal(err)
	}
	provider := newPieceCacheProvider(cache, root, 16, nil)
	instance, _ := provider.NewInstance("completed/piece")
	if err := instance.(*pieceCacheInstance).PutSized(bytes.NewReader([]byte("data")), 4); err != nil {
		t.Fatal(err)
	}
	reader, err := instance.Get()
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.Stat(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retiring piece remained available: %v", err)
	}
	path := filepath.Join(root, "completed", "piece")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("retiring open piece was unlinked: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("piece remains after final close: %v", err)
	}
}

func TestPieceBeingPublishedIsNotReadable(t *testing.T) {
	root := t.TempDir()
	cache, err := filecache.NewCache(root)
	if err != nil {
		t.Fatal(err)
	}
	provider := newPieceCacheProvider(cache, root, 16, nil)
	instance, _ := provider.NewInstance("completed/piece")
	input := &blockingPieceReader{started: make(chan struct{}), release: make(chan struct{})}
	written := make(chan error, 1)
	go func() {
		written <- instance.(*pieceCacheInstance).PutSized(input, 4)
	}()
	<-input.started
	if reader, err := instance.Get(); !errors.Is(err, os.ErrNotExist) {
		if reader != nil {
			_ = reader.Close()
		}
		t.Fatalf("publishing piece was readable: %v", err)
	}
	if _, err := instance.Stat(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publishing piece was reported complete: %v", err)
	}
	close(input.release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	reader, err := instance.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "data" {
		t.Fatalf("published piece = %q, %v", data, err)
	}
}

func TestPieceCacheRejectsSymlinkAndHardLink(t *testing.T) {
	root := t.TempDir()
	cache, err := filecache.NewCache(root)
	if err != nil {
		t.Fatal(err)
	}
	provider := newPieceCacheProvider(cache, root, 16, nil)
	instance, _ := provider.NewInstance("completed/piece")
	if err := instance.(*pieceCacheInstance).PutSized(bytes.NewReader([]byte("data")), 4); err != nil {
		t.Fatal(err)
	}
	piecePath := filepath.Join(root, "completed", "piece")
	if err := os.Link(piecePath, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if reader, err := instance.Get(); err == nil {
		_ = reader.Close()
		t.Fatal("hard-linked piece was opened")
	}
	if err := instance.Delete(); err == nil {
		t.Fatal("hard-linked piece was evicted")
	}
	if _, err := os.Stat(piecePath); err != nil {
		t.Fatalf("hard-linked piece lost its managed name: %v", err)
	}

	symlink, _ := provider.NewInstance("completed/symlink")
	if err := os.Symlink(piecePath, filepath.Join(root, "completed", "symlink")); err != nil {
		t.Fatal(err)
	}
	if reader, err := symlink.Get(); err == nil {
		_ = reader.Close()
		t.Fatal("symbolic-link piece was opened")
	}
}

func TestPieceDeleteDuringPublicationFinishesAfterWriter(t *testing.T) {
	root := t.TempDir()
	cache, err := filecache.NewCache(root)
	if err != nil {
		t.Fatal(err)
	}
	provider := newPieceCacheProvider(cache, root, 16, nil)
	instance, _ := provider.NewInstance("completed/piece")
	input := &blockingPieceReader{started: make(chan struct{}), release: make(chan struct{})}
	written := make(chan error, 1)
	go func() {
		written <- instance.(*pieceCacheInstance).PutSized(input, 4)
	}()
	<-input.started
	if err := instance.Delete(); err != nil {
		t.Fatal(err)
	}
	close(input.release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "completed", "piece")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired published piece remains: %v", err)
	}
}

type blockingPieceReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingPieceReader) Read(data []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return copy(data, "data"), io.EOF
}

func TestResourcePiecePromotionRemovesIncompleteChunksAfterReadersClose(t *testing.T) {
	root := t.TempDir()
	cache, err := filecache.NewCache(root)
	if err != nil {
		t.Fatal(err)
	}
	provider := newPieceCacheProvider(cache, root, 8, nil)
	client := storage.NewResourcePieces(provider)
	data := []byte("data")
	pieceHash := sha1.Sum(data)
	info := &metainfo.Info{PieceLength: 4, Length: 4, Pieces: pieceHash[:]}
	torrentStorage, err := client.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	piece := torrentStorage.PieceWithHash(info.Piece(0), g.Some(pieceHash[:]))
	if _, err := piece.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := piece.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	if completion := piece.Completion(); !completion.Ok || !completion.Complete {
		t.Fatalf("piece completion = %+v", completion)
	}
	var paths []string
	cache.WalkItems(func(info filecache.ItemInfo) { paths = append(paths, string(info.Path)) })
	if len(paths) != 1 || !strings.HasPrefix(paths[0], "completed/") {
		t.Fatalf("piece cache paths after promotion = %q", paths)
	}
}
