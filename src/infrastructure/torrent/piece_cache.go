package torrent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/anacrolix/missinggo/v2/filecache"
	"github.com/anacrolix/torrent/storage"
)

const pieceCacheDirName = ".pieces"

func validatePieceCacheCapacity(pieceLength, capacity int64) error {
	if capacity > 0 && pieceLength > capacity {
		return errors.New("torrent cache capacity is smaller than one torrent piece")
	}
	return nil
}

func newPieceCache(downloadRoot string, capacity int64) (storage.ClientImpl, error) {
	root := filepath.Join(downloadRoot, pieceCacheDirName)
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, fmt.Errorf("create torrent piece cache: %w", err)
	}
	cache, err := filecache.NewCache(root)
	if err != nil {
		return nil, fmt.Errorf("open torrent piece cache: %w", err)
	}

	if capacity > 0 {
		cache.SetCapacity(capacity)
		cache.TrimToCapacity()
	}

	return storage.NewResourcePiecesOpts(
		cache.AsResourceProvider(),
		storage.ResourcePiecesOpts{},
	), nil
}
