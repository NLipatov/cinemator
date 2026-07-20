package torrent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"cinemator/application"
)

// mediaCache is the ownership boundary for all reproducible media stored on
// disk. Policy remains in cache_cleaner.go; storage, leases, accounting, and
// physical admission are reached through this single coordinator.
type mediaCache struct {
	budget  *cacheBudget
	assets  *hlsAssetStore
	pieces  *pieceCacheProvider
	hlsDisk *diskBudget
}

func (c *mediaCache) synchronizeLifecycle(run func()) {
	if c == nil || c.budget == nil {
		run()
		return
	}
	c.budget.mu.Lock()
	defer c.budget.mu.Unlock()
	run()
}

func (c *mediaCache) publishHls(bytes, inodes uint64, publish func() error) error {
	var reservation *diskReservation
	if c != nil && c.hlsDisk != nil {
		var err error
		reservation, err = c.hlsDisk.Reserve(bytes, inodes)
		if err != nil {
			return err
		}
		defer reservation.Release()
	}
	return publish()
}

func (c *mediaCache) openHls(path string) (application.HlsAsset, error) {
	return c.assets.Open(path)
}

func (c *mediaCache) touchHls(path string) bool {
	return c.assets.Touch(path, hlsTouchInterval)
}

func (c *mediaCache) resetHls(root string) error {
	return c.assets.ResetTree(root)
}

func (c *mediaCache) retireHls(root string) error {
	err := c.assets.RetireTree(root)
	if errors.Is(err, errHlsAssetsBusy) {
		return nil
	}
	return err
}

func (c *mediaCache) retireDownloadHls(root, infoHash string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	prefix := infoHash + "_"
	for _, entry := range entries {
		if entry.IsDir() && len(entry.Name()) > len(prefix) && entry.Name()[:len(prefix)] == prefix {
			if err := c.retireHls(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *mediaCache) discardHlsStreams(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := c.retireHls(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *mediaCache) hasOpenHandles() bool {
	return c.assets.hasReaders() || c.pieces.hasLeases()
}

func (c *mediaCache) close() error {
	return c.assets.Close()
}

func (c *mediaCache) generationDiskHealthy() error {
	if c == nil || c.hlsDisk == nil {
		return nil
	}
	return c.hlsDisk.CheckFloor()
}
