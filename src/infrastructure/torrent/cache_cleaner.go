package torrent

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

func (m *manager) reserveHlsGeneration(segments int) (func(), error) {
	limit := m.settings.MaxCacheBytes()
	if limit <= 0 {
		return func() {}, nil
	}
	required := estimatedHlsWindowBytes(segments, m.settings.HlsSegmentDuration())
	if required > limit {
		return nil, fmt.Errorf("HLS window requires about %d bytes but the cache limit is %d", required, limit)
	}

	m.cacheMu.Lock()
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
	m.hlsReserved += required
	m.cacheMu.Unlock()

	return func() {
		m.cacheMu.Lock()
		m.hlsReserved = max(0, m.hlsReserved-required)
		m.cacheMu.Unlock()
	}, nil
}

func estimatedHlsWindowBytes(segments int, segmentDuration time.Duration) int64 {
	const maxMuxedBitsPerSecond = int64(5_500_000)
	if segments <= 0 || segmentDuration <= 0 {
		return 0
	}
	base := maxMuxedBitsPerSecond * int64(segments) * int64(segmentDuration) / int64(8*time.Second)
	return base + base/4
}

func (m *manager) trimHlsCache(target int64) (int64, error) {

	active, pinned := m.hlsCacheProtection()
	root := m.settings.HlsPath()
	var total int64
	var candidates []hlsCacheItem
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		size := info.Size()
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
			if isHlsGenerationTemporary(rel) || !isGeneratedHlsAsset(filepath.Base(path)) {
				return nil
			}
		}
		candidates = append(candidates, hlsCacheItem{path: path, size: size, last: info.ModTime()})
		return nil
	})

	if total <= target {
		return total, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].last.Before(candidates[j].last)
	})
	for _, item := range candidates {
		if total <= target {
			break
		}
		if err := os.Remove(item.path); err != nil && !os.IsNotExist(err) {
			log.Printf("enforceCacheLimit: failed to remove %s: %v", item.path, err)
			continue
		}
		total -= item.size
		log.Printf("enforceCacheLimit: removed %s (freed %d bytes)", item.path, item.size)
	}

	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if entry.IsDir() {
			_ = os.Remove(filepath.Join(root, entry.Name()))
		}
	}
	if total > target {
		return total, fmt.Errorf("cache is %d bytes above target because remaining assets are active or could not be removed", total-target)
	}
	return total, nil
}

func isHlsGenerationTemporary(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return len(parts) > 1 && strings.HasPrefix(parts[1], ".generating-")
}

func (m *manager) hlsCacheProtection() (active, pinned map[string]struct{}) {
	active = make(map[string]struct{})
	pinned = make(map[string]struct{})
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, stream := range m.active {
		active[key.dirName()] = struct{}{}
		stream.mtx.Lock()
		for job := range stream.videoJobs {
			pinSegmentJob(pinned, stream.paths.outDir, job, videoSegmentName)
			if !jobFinished(job) {
				window := filepath.Join(stream.paths.outDir, "window_"+formatSegmentIndex(job.begin)+".m3u8")
				pinned[window] = struct{}{}
				pinned[window+".tmp"] = struct{}{}
			}
		}
		for job := range stream.subtitleJobs {
			pinSegmentJob(pinned, stream.paths.outDir, job, subtitleSegmentName)
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
		strings.HasSuffix(name, ".vtt") ||
		strings.HasSuffix(name, ".tmp") ||
		(strings.HasPrefix(name, "window_") && strings.HasSuffix(name, ".m3u8"))
}
