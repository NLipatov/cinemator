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

	m.mu.Lock()
	defer m.mu.Unlock()
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
		filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if info, err := d.Info(); err == nil && !info.IsDir() {
				size += info.Size()
			}
			return nil
		})
		total += size
		it := item{path: dir, size: size, last: time.Now()}
		if s, ok := m.active[key]; ok {
			it.last = s.lastView
			it.active = true
		} else if info, err := os.Stat(dir); err == nil {
			it.last = info.ModTime()
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
		if err := os.RemoveAll(it.path); err != nil {
			log.Printf("enforceCacheLimit: failed to remove %s: %v", it.path, err)
			continue
		}
		total -= it.size
		log.Printf("enforceCacheLimit: removed %s (freed %d bytes)", it.path, it.size)
	}
}
