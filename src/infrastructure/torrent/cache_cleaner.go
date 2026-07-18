package torrent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
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

func (m *manager) enforceCacheLimit() {
	limit := m.settings.MaxCacheBytes()
	if limit <= 0 {
		return
	}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if _, err := m.trimHlsCache(max(0, limit-m.hlsReserved)); err != nil {
		log.Printf("enforceCacheLimit: %v", err)
	}
}

func (m *manager) reserveHlsGeneration(duration time.Duration, bitrate int64) (func(), error) {
	limit := m.settings.MaxCacheBytes()
	required := estimatedHlsWindowBytes(duration, bitrate)
	if limit > 0 && required > limit {
		return nil, fmt.Errorf("HLS window requires about %d bytes but the cache limit is %d", required, limit)
	}

	m.cacheMu.Lock()
	if limit > 0 {
		target := limit - m.hlsReserved - required
		if target < 0 {
			reserved := m.hlsReserved
			m.cacheMu.Unlock()
			return nil, fmt.Errorf("cannot reserve %d bytes in HLS cache: %d bytes are already reserved", required, reserved)
		}
		total, err := m.trimHlsCache(target)
		if err == nil && total > target {
			err = fmt.Errorf("cannot reserve %d bytes in HLS cache: %d protected bytes remain", required, total)
		}
		if err != nil {
			m.cacheMu.Unlock()
			return nil, err
		}
	}
	var physical *diskReservation
	if m.hlsDisk != nil {
		reservation, err := m.hlsDisk.Reserve(uint64(required), estimatedHlsWindowInodes(duration, m.settings.HlsSegmentDuration()))
		if err != nil {
			m.cacheMu.Unlock()
			return nil, err
		}
		physical = reservation
	}
	m.hlsReserved += required
	m.cacheMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.cacheMu.Lock()
			m.hlsReserved = max(0, m.hlsReserved-required)
			m.cacheMu.Unlock()
			physical.Release()
		})
	}, nil
}

func estimatedHlsWindowBytes(duration time.Duration, bitrate int64) int64 {
	const maxMuxedBitsPerSecond = int64(5_500_000)
	if duration <= 0 {
		return 0
	}
	bitrate = max(bitrate, maxMuxedBitsPerSecond)
	base := bitrate / 8 * int64(duration/time.Second)
	return base + base/4
}

func estimatedHlsWindowInodes(duration, segmentDuration time.Duration) uint64 {
	if duration <= 0 || segmentDuration <= 0 {
		return 16
	}
	segments := uint64((duration + segmentDuration - 1) / segmentDuration)
	return segments*2 + 16
}

func (m *manager) trimHlsCache(target int64) (int64, error) {

	active, pinned := m.hlsCacheProtection()
	root := m.settings.HlsPath()
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
		if _, ok := pinned[path]; ok {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		dir := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		if _, isActive := active[dir]; isActive {
			if _, hasPublisher := pinned[filepath.Join(root, dir)]; hasPublisher {
				return nil
			}
			if isHlsGenerationTemporary(rel) || !isGeneratedHlsAsset(filepath.Base(path)) {
				return nil
			}
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
	for _, item := range candidates {
		if total <= target {
			break
		}
		var removed bool
		var err error
		rel, relErr := filepath.Rel(root, item.path)
		parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
		if relErr == nil && len(parts) == 2 {
			_, isActive := active[parts[0]]
			if isActive && isImmutableHlsAsset(filepath.Base(item.path)) {
				removed, err = m.evictActiveHlsAsset(parts[0], item.path)
			} else {
				removed, err = m.assets.TryEvict(item.path)
			}
		} else {
			removed, err = m.assets.TryEvict(item.path)
		}
		if err != nil {
			log.Printf("enforceCacheLimit: failed to remove %s: %v", item.path, err)
			continue
		}
		if !removed {
			continue
		}
		total -= item.size
		removedFiles++
		unlinkedBytes += item.size
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
	return m.hlsDisk.CheckFloor()
}

func (m *manager) checkHlsCacheLimit() error {
	limit := m.settings.MaxCacheBytes()
	if limit <= 0 {
		return nil
	}
	var total int64
	err := filepath.WalkDir(m.settings.HlsPath(), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() || path == filepath.Join(m.settings.HlsPath(), cacheOwnerLockName) {
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

func isHlsGenerationTemporary(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return len(parts) > 1 && (strings.HasPrefix(parts[1], ".generating-") || strings.HasPrefix(parts[1], ".remuxing-"))
}

func (m *manager) hlsCacheProtection() (active, pinned map[string]struct{}) {
	active = make(map[string]struct{})
	pinned = make(map[string]struct{})
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, stream := range m.active {
		active[key.dirName()] = struct{}{}
		stream.mtx.Lock()
		publishing := false
		if stream.ready != nil {
			select {
			case <-stream.ready:
			default:
				publishing = true
			}
		}
		for job := range stream.videoJobs {
			pinSegmentJob(pinned, stream.paths.outDir, job, videoSegmentName)
			if !jobFinished(job) {
				publishing = true
				window := filepath.Join(stream.paths.outDir, "window_"+formatSegmentIndex(job.begin)+".m3u8")
				pinned[window] = struct{}{}
				pinned[window+".tmp"] = struct{}{}
			}
		}
		for job := range stream.subtitleJobs {
			pinSegmentJob(pinned, stream.paths.outDir, job, subtitleSegmentName)
			publishing = publishing || !jobFinished(job)
		}
		if publishing {
			pinned[stream.paths.outDir] = struct{}{}
		}
		stream.mtx.Unlock()
	}
	return active, pinned
}

func pinSegmentJob(pinned map[string]struct{}, outDir string, job *segmentJob, name func(int) string) {
	if job == nil || jobFinished(job) {
		return
	}
	for index := job.begin; index < job.end; index++ {
		path := filepath.Join(outDir, name(index))
		pinned[path] = struct{}{}
		pinned[path+".tmp"] = struct{}{}
	}
}

func isGeneratedHlsAsset(name string) bool {
	return strings.HasSuffix(name, ".ts") ||
		strings.HasSuffix(name, ".m4s") ||
		strings.HasSuffix(name, ".mp4") ||
		strings.HasSuffix(name, ".vtt") ||
		strings.HasSuffix(name, ".tmp") ||
		(strings.HasPrefix(name, "window_") && strings.HasSuffix(name, ".m3u8"))
}

func isImmutableHlsAsset(name string) bool {
	return strings.HasSuffix(name, ".ts") ||
		strings.HasSuffix(name, ".m4s") ||
		strings.HasSuffix(name, ".mp4") ||
		strings.HasSuffix(name, ".vtt")
}
