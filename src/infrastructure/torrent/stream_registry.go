package torrent

import (
	"context"
	"fmt"
	"log"
	"os"
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
	m.cleanupExpiredDownloads(context.Background())
}

func (m *manager) cleanup(key streamKey) {
	m.cleanupMatching(key, nil, 0, false)
}

func (m *manager) cleanupIfCurrent(key streamKey, expected *streamInfo) {
	m.cleanupMatching(key, expected, 0, false)
}

func (m *manager) cleanupIfCurrentRun(key streamKey, expected *streamInfo, runID uint64) {
	m.cleanupMatching(key, expected, runID, true)
}

func (m *manager) cleanupMatching(key streamKey, expected *streamInfo, runID uint64, checkRun bool) {
	m.mu.Lock()
	s, shouldDrop, cleaned := m.cleanupMatchingLocked(key, expected, runID, checkRun)
	if !cleaned {
		m.mu.Unlock()
		return
	}
	m.finishStreamCleanupLocked(key, s, shouldDrop)
	m.mu.Unlock()
}

func (m *manager) cleanupMatchingLocked(key streamKey, expected *streamInfo, runID uint64, checkRun bool) (*streamInfo, bool, bool) {
	s, ok := m.active[key]
	if !ok {
		if expected == nil {
			log.Printf("cleanup called, but no active stream found: key=%v", key)
		}
		return nil, false, false
	}
	if expected != nil && s != expected {
		return nil, false, false
	}
	if checkRun && !s.isCurrentRun(runID) {
		return nil, false, false
	}
	if s.cancel != nil {
		s.cancel()
	}
	shouldDrop := false
	if cnt, ok := m.torrents[key.InfoHash]; ok {
		if cnt <= 1 {
			delete(m.torrents, key.InfoHash)
			shouldDrop = true
		} else {
			m.torrents[key.InfoHash] = cnt - 1
		}
	}
	delete(m.active, key)
	return s, shouldDrop, true
}

func (m *manager) finishStreamCleanupLocked(key streamKey, s *streamInfo, shouldDrop bool) {
	s.source.Close()
	if s.runDone != nil {
		<-s.runDone
	}
	log.Printf("Cleaning up stream: key=%v, dir=%s", key, s.paths.outDir)
	if err := os.RemoveAll(s.paths.outDir); err != nil {
		log.Printf("Failed to cleanup directory: %s, err=%v", s.paths.outDir, err)
	}
	if s.file != nil {
		s.file.SetPriority(torrent.PiecePriorityNone)
	}
	m.notifyDownloadsChanged()
	log.Printf("Stream cleaned up: key=%v", key)
	if shouldDrop && s.torrent != nil {
		log.Printf("Dropping torrent: %s", key.InfoHash)
		s.torrent.Drop()
	}
}

func (m *manager) releaseStartupWaiter(key streamKey, s *streamInfo, runID uint64, requestCanceled bool) bool {
	m.mu.Lock()
	current, active := m.active[key]
	s.mtx.Lock()
	registered := s.startupWaiters > 0
	if registered {
		s.startupWaiters--
	}
	abandoned := registered && requestCanceled &&
		active && current == s && runID == s.runID &&
		s.startupWaiters == 0 && !s.viewerSeen
	s.mtx.Unlock()

	cleaned := false
	var cleanedStream *streamInfo
	var shouldDrop bool
	if abandoned {
		cleanedStream, shouldDrop, cleaned = m.cleanupMatchingLocked(key, s, runID, true)
	}
	if cleaned {
		m.finishStreamCleanupLocked(key, cleanedStream, shouldDrop)
	}
	m.mu.Unlock()
	return cleaned
}

func (m *manager) pauseStream(key streamKey) {
	m.mu.Lock()
	s, ok := m.active[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	if s.paused || s.completed || !s.running {
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
	m.notifyDownloadsChanged()
	log.Printf("Paused stream due to inactivity: key=%v", key)
}

func (m *manager) startConversionLocked(key streamKey, s *streamInfo) error {
	if s.running || s.completed {
		return nil
	}
	if s.runDone != nil {
		<-s.runDone
	}
	if err := resetStreamOutput(s.paths); err != nil {
		return fmt.Errorf("reset stream output %s: %w", s.paths.outDir, err)
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.paused = false
	s.running = true
	s.file.SetPriority(torrent.PiecePriorityHigh)
	runID := s.beginRun()
	m.launchConversion(streamCtx, key, s, runID)
	return nil
}

func resetStreamOutput(paths streamPaths) error {
	if err := os.RemoveAll(paths.outDir); err != nil {
		return err
	}
	return os.MkdirAll(paths.outDir, 0755)
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
		s.viewerSeen = true
		s.mtx.Unlock()
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
			playable := false
			select {
			case <-s.playable:
				playable = true
			default:
			}
			idle := playable && s.running && !s.completed && now.Sub(s.lastView) > idlePauseTimeout
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
		m.cleanupExpiredDownloads(context.Background())
	}
}
