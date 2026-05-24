package torrent

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/anacrolix/torrent"
)

const idlePauseTimeout = 15 * time.Minute

func (m *manager) CleanupStreams() {
	now := time.Now()
	m.mu.Lock()
	for key, s := range m.active {
		s.mtx.Lock()
		noViewers := now.Sub(s.lastView) > m.settings.ViewerTimeout()
		s.mtx.Unlock()
		if noViewers {
			go m.cleanup(key)
		}
	}
	m.mu.Unlock()
	m.enforceCacheLimit()
}

func (m *manager) cleanup(key streamKey) {
	m.cleanupMatching(key, nil)
}

func (m *manager) cleanupIfCurrent(key streamKey, expected *streamInfo) {
	m.cleanupMatching(key, expected)
}

func (m *manager) cleanupMatching(key streamKey, expected *streamInfo) {
	m.mu.Lock()
	s, ok := m.active[key]
	if !ok {
		if expected == nil {
			log.Printf("cleanup called, but no active stream found: key=%v", key)
		}
		m.mu.Unlock()
		return
	}
	if expected != nil && s != expected {
		m.mu.Unlock()
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.source.Close()
	shouldDrop := false
	if cnt, ok := m.torrents[key.InfoHash]; ok {
		if cnt <= 1 {
			delete(m.torrents, key.InfoHash)
			shouldDrop = true
		} else {
			m.torrents[key.InfoHash] = cnt - 1
		}
	}
	log.Printf("Cleaning up stream: key=%v, dir=%s", key, s.paths.outDir)
	err := os.RemoveAll(s.paths.outDir)
	if err != nil {
		log.Printf("Failed to cleanup directory: %s, err=%v", s.paths.outDir, err)
	}
	if s.file != nil {
		s.file.SetPriority(0)
	}
	delete(m.active, key)
	m.mu.Unlock()
	log.Printf("Stream cleaned up: key=%v", key)
	if shouldDrop {
		log.Printf("Dropping torrent: %s", key.InfoHash)
		s.torrent.Drop()
		dlDir := filepath.Join(m.settings.DownloadPath(), key.InfoHash)
		if rmErr := os.RemoveAll(dlDir); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Printf("Failed to remove download dir %s: %v", dlDir, rmErr)
		}
	}
}

func (m *manager) pauseStream(key streamKey) {
	m.mu.Lock()
	s, ok := m.active[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	if s.paused {
		m.mu.Unlock()
		return
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.paused = true
	s.running = false
	s.file.SetPriority(torrent.PiecePriorityNone)
	m.mu.Unlock()
	log.Printf("Paused stream due to inactivity: key=%v", key)
}

func (m *manager) startConversionLocked(key streamKey, s *streamInfo) {
	if s.running {
		return
	}
	if err := os.MkdirAll(s.paths.outDir, 0755); err != nil {
		log.Printf("startConversionLocked: failed to ensure dir %s: %v", s.paths.outDir, err)
		return
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.paused = false
	s.running = true
	s.file.SetPriority(torrent.PiecePriorityHigh)
	s.resetReady()
	m.launchConversion(streamCtx, streamCtx, key, s)
}

func (m *manager) TouchStream(_ context.Context, dirName string) {
	key, err := parseStreamDir(dirName)
	if err != nil {
		return
	}
	m.mu.Lock()
	if s, ok := m.active[key]; ok {
		s.mtx.Lock()
		s.lastView = time.Now()
		needResume := s.paused || s.cancel == nil
		s.mtx.Unlock()
		if needResume {
			m.startConversionLocked(key, s)
		}
	}
	m.mu.Unlock()
}

func (m *manager) viewerWatcher() {
	ticker := time.NewTicker(time.Minute / 3)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		m.mu.Lock()
		for key, s := range m.active {
			s.mtx.Lock()
			readyClosed := false
			select {
			case <-s.ready:
				readyClosed = true
			default:
			}
			idle := readyClosed && now.Sub(s.lastView) > idlePauseTimeout
			noViewers := now.Sub(s.lastView) > m.settings.ViewerTimeout()
			s.mtx.Unlock()
			if idle && !s.paused {
				go m.pauseStream(key)
			}
			if noViewers {
				log.Printf("Viewer timeout detected, cleaning up stream: key=%v", key)
				go m.cleanup(key)
			}
		}
		m.mu.Unlock()
		m.enforceCacheLimit()
	}
}
