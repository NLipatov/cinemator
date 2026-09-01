package torrent

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func (m *manager) enforceCacheLimit() {
	limit := m.settings.MaxCacheBytes()
	if limit <= 0 {
		return
	}

	type item struct {
		path   string
		size   int64
		last   time.Time
		active bool
	}

	var (
		total int64
		items []item
	)
	root := m.settings.HlsPath()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		key, err := parseStreamDir(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(root, e.Name())
		var size int64
		if err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.IsDir() {
				size += info.Size()
			}
			return nil
		}); err != nil {
			log.Printf("enforceCacheLimit: failed to inspect %s: %v", dir, err)
			return
		}
		total += size
		it := item{path: dir, size: size, last: time.Now()}
		m.mu.Lock()
		if s, ok := m.active[key]; ok {
			s.mtx.Lock()
			it.last = s.lastView
			s.mtx.Unlock()
			it.active = true
		} else if m.streamOps[key] != nil {
			it.active = true
		}
		m.mu.Unlock()
		if !it.active {
			if info, err := os.Stat(dir); err == nil {
				it.last = info.ModTime()
			}
		}
		items = append(items, it)
	}

	if total <= limit {
		return
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].last.Before(items[j].last)
	})

	for _, it := range items {
		if total <= limit {
			break
		}
		if it.active {
			continue
		}
		key, err := parseStreamDir(filepath.Base(it.path))
		if err != nil {
			continue
		}
		m.mu.Lock()
		if m.active[key] != nil || m.streamOps[key] != nil {
			m.mu.Unlock()
			continue
		}
		operationDone := m.reserveStreamOperationLocked(key)
		m.mu.Unlock()
		if err := os.RemoveAll(it.path); err != nil {
			m.finishStreamOperation(key, operationDone)
			log.Printf("enforceCacheLimit: failed to remove %s: %v", it.path, err)
			return
		}
		m.finishStreamOperation(key, operationDone)
		total -= it.size
		log.Printf("enforceCacheLimit: removed %s (freed %d bytes)", it.path, it.size)
		m.notifyDownloadsChanged()
	}
}
