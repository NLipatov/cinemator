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

type pieceCacheStorage struct {
	storage.ClientImpl
	provider *pieceCacheProvider
}

func validatePieceCacheCapacity(pieceLength, capacity int64) error {
	if capacity > 0 && pieceLength > capacity/2 {
		return errors.New("torrent cache capacity must hold an incomplete piece and its verified copy")
	}
	return nil
}

func newPieceCache(downloadRoot string, budget *cacheBudget, disk *diskBudget) (*pieceCacheStorage, error) {
	root, err := filepath.Abs(filepath.Join(downloadRoot, pieceCacheDirName))
	if err != nil {
		return nil, fmt.Errorf("resolve torrent piece cache: %w", err)
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		err = os.MkdirAll(root, 0755)
	} else if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		err = errors.New("torrent piece cache root is not a directory")
	}
	if err != nil {
		return nil, fmt.Errorf("create torrent piece cache: %w", err)
	}
	// Incomplete chunks belong to the previous fenced process. Complete pieces
	// are content-addressed and remain reusable; partial writes do not.
	if err := os.RemoveAll(filepath.Join(root, "incompleted")); err != nil {
		return nil, fmt.Errorf("discard incomplete torrent pieces: %w", err)
	}
	cache, err := filecache.NewCache(root)
	if err != nil {
		return nil, fmt.Errorf("open torrent piece cache: %w", err)
	}

	provider := newPieceCacheProvider(cache, root, budget, disk)
	if err := provider.trimToCapacity(); err != nil {
		return nil, fmt.Errorf("trim torrent piece cache: %w", err)
	}

	opts := storage.ResourcePiecesOpts{}
	if budget.limit > 0 {
		capacityFn := func() (int64, bool) { return budget.limit, true }
		opts.Capacity = &capacityFn
	}
	return &pieceCacheStorage{ClientImpl: storage.NewResourcePiecesOpts(provider, opts), provider: provider}, nil
}
