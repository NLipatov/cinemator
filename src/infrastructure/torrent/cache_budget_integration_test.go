package torrent

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/anacrolix/missinggo/v2/filecache"
	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestPieceEvictionUpdatesTorrentCompletion(t *testing.T) {
	root := t.TempDir()
	cacheRoot := filepath.Join(root, "pieces")
	cache, err := filecache.NewCache(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	provider := newPieceCacheProvider(cache, cacheRoot, newCacheBudget(4), nil)
	cfg := atorrent.TestingConfig(t)
	cfg.DefaultStorage = storage.NewResourcePieces(provider)
	client, err := atorrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	data := []byte("data")
	pieceHash := sha1.Sum(data)
	info := metainfo.Info{Name: "movie", PieceLength: 4, Length: 4, Pieces: pieceHash[:]}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := provider.NewInstance("completed/" + hex.EncodeToString(pieceHash[:]))
	if err != nil {
		t.Fatal(err)
	}
	if err := completed.(*pieceCacheInstance).PutSized(bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	tor, _ := client.AddTorrentOpt(atorrent.AddTorrentOpts{
		InfoBytes:                infoBytes,
		InfoHash:                 metainfo.HashBytes(infoBytes),
		DisableInitialPieceCheck: true,
		DisallowDataDownload:     true,
	})
	tor.Piece(0).UpdateCompletion()
	if got := tor.BytesCompleted(); got != int64(len(data)) {
		t.Fatalf("completed bytes before eviction = %d", got)
	}

	m := &manager{
		media:       &mediaCache{pieces: provider},
		pieceRefs:   make(map[string][]cachedPieceRef),
		pieceHashes: make(map[*atorrent.Torrent][]string),
	}
	provider.onEvict = m.syncEvictedPieces
	m.indexTorrentPieces(tor)
	replacement, err := provider.NewInstance("completed/replacement")
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.(*pieceCacheInstance).PutSized(bytes.NewReader([]byte("next")), 4); err != nil {
		t.Fatal(err)
	}
	if got := tor.BytesCompleted(); got != 0 {
		t.Fatalf("completed bytes after eviction = %d, want 0", got)
	}
}
