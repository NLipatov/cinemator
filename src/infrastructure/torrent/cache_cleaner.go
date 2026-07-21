package torrent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type hlsCacheItem struct {
	path string
	size int64
	last time.Time
}

type cacheBudget struct {
	mu          sync.Mutex
	limit       int64
	hlsBytes    int64
	hlsReserved int64
}

func newCacheBudget(limit int64) *cacheBudget {
	return &cacheBudget{limit: limit}
}

func (m *manager) enforceCacheLimit() {
	if m.media == nil {
		return
	}
	if err := m.media.enforce(
		m.settings.HlsPath(),
		m.hlsCacheProtection,
	); err != nil {
		log.Printf("enforceCacheLimit: %v", err)
	}
}

func (m *manager) reserveHlsGeneration(duration time.Duration, bitrate int64) (func(), error) {
	return m.media.reserveHlsGeneration(
		duration,
		bitrate,
		m.settings.HlsSegmentDuration(),
		m.settings.HlsPath(),
		m.hlsCacheProtection,
	)
}

type hlsCacheProtection func() map[string]struct{}

func (c *mediaCache) enforce(root string, protection hlsCacheProtection) error {
	if c.budget == nil || c.budget.limit <= 0 {
		return nil
	}
	c.budget.mu.Lock()
	_, evicted, err := c.trimSharedCacheLocked(max(0, c.budget.limit-c.budget.hlsReserved), root, protection)
	c.budget.mu.Unlock()
	c.notifyPieceEvictions(evicted)
	return err
}

func (c *mediaCache) reserveHlsGeneration(
	duration time.Duration,
	bitrate int64,
	segmentDuration time.Duration,
	root string,
	protection hlsCacheProtection,
) (func(), error) {
	limit := c.budget.limit
	required := estimatedHlsWindowBytes(duration, bitrate)
	if limit > 0 && required > limit {
		return nil, fmt.Errorf("HLS window requires about %d bytes but the shared cache limit is %d", required, limit)
	}

	c.budget.mu.Lock()
	var evicted []string
	if limit > 0 {
		target := limit - c.budget.hlsReserved - required
		if target < 0 {
			reserved := c.budget.hlsReserved
			c.budget.mu.Unlock()
			return nil, fmt.Errorf("cannot reserve %d HLS bytes: %d shared cache bytes are already reserved", required, reserved)
		}
		total, removed, err := c.trimSharedCacheLocked(target, root, protection)
		evicted = append(evicted, removed...)
		if err == nil && total > target {
			err = fmt.Errorf("cannot reserve %d HLS bytes: %d protected shared cache bytes remain", required, total)
		}
		if err != nil {
			c.budget.mu.Unlock()
			c.notifyPieceEvictions(evicted)
			return nil, err
		}
	}
	var physical *diskReservation
	if c.hlsDisk != nil {
		reservation, err := c.hlsDisk.Reserve(uint64(required), estimatedHlsWindowInodes(duration, segmentDuration))
		if err != nil {
			c.budget.mu.Unlock()
			c.notifyPieceEvictions(evicted)
			return nil, err
		}
		physical = reservation
	}
	c.budget.hlsReserved += required
	c.budget.mu.Unlock()
	c.notifyPieceEvictions(evicted)

	var once sync.Once
	return func() {
		once.Do(func() {
			c.budget.mu.Lock()
			c.budget.hlsReserved = max(0, c.budget.hlsReserved-required)
			if total, err := c.trimHlsCache(math.MaxInt64, root, protection); err == nil {
				c.budget.hlsBytes = total
			} else {
				c.budget.hlsBytes += required
			}
			c.budget.mu.Unlock()
			physical.Release()
		})
	}, nil
}

// trimSharedCacheLocked gives generated HLS priority over reproducible source
// pieces. Pieces can still consume the entire budget while HLS is absent.
func (c *mediaCache) trimSharedCacheLocked(target int64, root string, protection hlsCacheProtection) (int64, []string, error) {
	hlsBytes, err := c.trimHlsCache(math.MaxInt64, root, protection)
	if err != nil {
		return 0, nil, err
	}
	c.budget.hlsBytes = hlsBytes
	pieceBytes := int64(0)
	var evicted []string
	if c.pieces != nil {
		pieceBytes = c.pieces.usedBytes()
		if hlsBytes+pieceBytes > target {
			pieceBytes, evicted, _ = c.pieces.trimTo(max(0, target-hlsBytes))
		}
	}
	if hlsBytes+pieceBytes > target {
		hlsBytes, err = c.trimHlsCache(max(0, target-pieceBytes), root, protection)
		c.budget.hlsBytes = hlsBytes
		if err != nil {
			return hlsBytes + pieceBytes, evicted, err
		}
	}
	total := hlsBytes + pieceBytes
	if total > target {
		return total, evicted, fmt.Errorf("shared cache is %d bytes above target because remaining data is active", total-target)
	}
	return total, evicted, nil
}

func (c *mediaCache) notifyPieceEvictions(locations []string) {
	if c != nil && c.pieces != nil {
		c.pieces.notifyEvicted(locations)
	}
}

func estimatedHlsWindowBytes(duration time.Duration, bitrate int64) int64 {
	const maxMuxedBitsPerSecond = int64(5_500_000)
	if duration <= 0 {
		return 0
	}
	bitrate = max(bitrate, maxMuxedBitsPerSecond)
	seconds := int64(math.Ceil(duration.Seconds()))
	bytesPerSecond := bitrate / 8
	if bitrate%8 != 0 {
		bytesPerSecond++
	}
	if seconds > math.MaxInt64/bytesPerSecond {
		return math.MaxInt64
	}
	base := bytesPerSecond * seconds
	// Compatibility output uses a two-second VBV buffer. The remaining 25%
	// covers muxing, init data and filesystem allocation granularity.
	buffer := bytesPerSecond * 2
	overhead := base / 4
	if base%4 != 0 {
		overhead++
	}
	if base > math.MaxInt64-buffer || base+buffer > math.MaxInt64-overhead {
		return math.MaxInt64
	}
	return base + buffer + overhead
}

func estimatedHlsWindowInodes(duration, segmentDuration time.Duration) uint64 {
	if duration <= 0 || segmentDuration <= 0 {
		return 16
	}
	segments := uint64((duration + segmentDuration - 1) / segmentDuration)
	return segments*2 + 16
}

func (c *mediaCache) trimHlsCache(target int64, root string, protection hlsCacheProtection) (int64, error) {
	active := protection()
	var total int64
	var candidates []hlsCacheItem
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if path == filepath.Join(root, cacheOwnerLockName) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		size := allocatedFileSize(info)
		total += size
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		dir := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		if _, isActive := active[dir]; isActive {
			return nil
		}
		candidates = append(candidates, hlsCacheItem{path: path, size: size, last: info.ModTime()})
		return nil
	})
	if walkErr != nil {
		return 0, fmt.Errorf("scan HLS cache: %w", walkErr)
	}

	if total <= target {
		return total, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].last.Before(candidates[j].last)
	})
	var removedFiles int
	var unlinkedBytes int64
	recordRemoved := func(item hlsCacheItem) {
		total -= item.size
		removedFiles++
		unlinkedBytes += item.size
	}
	for _, item := range candidates {
		if total <= target {
			break
		}
		removed, err := c.assets.TryEvict(item.path)
		if err != nil {
			log.Printf("enforceCacheLimit: failed to remove %s: %v", item.path, err)
			continue
		}
		if removed {
			recordRemoved(item)
		}
	}
	if removedFiles > 0 {
		log.Printf("enforceCacheLimit: unlinked %d closed files (%d allocated bytes removed from logical cache accounting)", removedFiles, unlinkedBytes)
	}
	if total > target {
		return total, fmt.Errorf("cache is %d bytes above target because remaining assets are active or could not be removed", total-target)
	}
	return total, nil
}

func (m *manager) monitorHlsResources(ctx context.Context, cancel context.CancelCauseFunc) <-chan error {
	result := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				result <- nil
				return
			case <-ticker.C:
				if err := m.checkHlsResources(); err != nil {
					result <- err
					cancel(err)
					return
				}
			}
		}
	}()
	return result
}

func (m *manager) checkHlsResources() error {
	return m.media.generationDiskHealthy()
}

func (m *manager) checkHlsCacheLimit() error {
	return m.media.checkHlsLimit(m.settings.HlsPath())
}

func (c *mediaCache) checkHlsLimit(root string) error {
	limit := c.budget.limit
	if limit <= 0 {
		return nil
	}
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() || path == filepath.Join(root, cacheOwnerLockName) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		total += allocatedFileSize(info)
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan HLS cache: %w", err)
	}
	if total > limit {
		return fmt.Errorf("HLS cache hard limit crossed: allocated=%d limit=%d", total, limit)
	}
	return nil
}

func (m *manager) hlsCacheProtection() map[string]struct{} {
	active := make(map[string]struct{})
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.active {
		active[key.dirName()] = struct{}{}
	}
	return active
}
