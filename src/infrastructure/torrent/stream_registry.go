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
			go func(key streamKey) {
				if err := m.cleanup(context.Background(), key); err != nil {
					log.Printf("Failed to clean up stream: key=%v, err=%v", key, err)
				}
			}(key)
		}
	}
	m.mu.Unlock()
	m.enforceCacheLimit()
	m.cleanupExpiredDownloads(context.Background())
}

func (m *manager) cleanup(ctx context.Context, key streamKey) error {
	return m.cleanupMatching(ctx, key, nil, 0, false)
}

func (m *manager) cleanupIfCurrentRun(key streamKey, expected *streamInfo, runID uint64) {
	if err := m.cleanupMatching(context.Background(), key, expected, runID, true); err != nil {
		log.Printf("Failed to clean up stream: key=%v, err=%v", key, err)
	}
}

func (m *manager) cleanupMatching(ctx context.Context, key streamKey, expected *streamInfo, runID uint64, checkRun bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		m.mu.Lock()
		s, operationDone, started := m.cleanupMatchingLocked(key, expected, runID, checkRun)
		m.mu.Unlock()
		if started {
			go m.finishStreamCleanup(key, s, operationDone)
			return waitForDone(ctx, operationDone)
		}
		if operationDone != nil {
			if err := waitForDone(ctx, operationDone); err != nil {
				return err
			}
			continue
		}
		return nil
	}
}

func (m *manager) cleanupMatchingLocked(key streamKey, expected *streamInfo, runID uint64, checkRun bool) (*streamInfo, chan struct{}, bool) {
	if operationDone := m.streamOps[key]; operationDone != nil {
		return nil, operationDone, false
	}
	s, ok := m.active[key]
	if !ok {
		if expected == nil {
			log.Printf("cleanup called, but no active stream found: key=%v", key)
		}
		return nil, nil, false
	}
	if expected != nil && s != expected {
		return nil, nil, false
	}
	if checkRun && !s.isCurrentRun(runID) {
		return nil, nil, false
	}
	if s.cancel != nil {
		s.cancel()
	}
	operationDone := m.reserveStreamOperationLocked(key)
	delete(m.active, key)
	return s, operationDone, true
}

func (m *manager) finishStreamCleanup(key streamKey, s *streamInfo, operationDone chan struct{}) {
	s.source.Close()
	if s.runDone != nil {
		<-s.runDone
	}
	log.Printf("Cleaning up stream: key=%v, dir=%s", key, s.paths.outDir)
	if err := os.RemoveAll(s.paths.outDir); err != nil {
		log.Printf("Failed to cleanup directory: %s, err=%v", s.paths.outDir, err)
	}

	m.mu.Lock()
	fileInUse := false
	for activeKey := range m.active {
		if activeKey.InfoHash == key.InfoHash && activeKey.Index == key.Index {
			fileInUse = true
			break
		}
	}
	m.mu.Unlock()
	if s.file != nil && !fileInUse {
		s.file.SetPriority(torrent.PiecePriorityNone)

		m.mu.Lock()
		for activeKey := range m.active {
			if activeKey.InfoHash == key.InfoHash && activeKey.Index == key.Index {
				fileInUse = true
				break
			}
		}
		m.mu.Unlock()
		if fileInUse {
			s.file.SetPriority(torrent.PiecePriorityHigh)
		}
	}
	if dropDone := m.releaseTorrent(key.InfoHash, s.torrent); dropDone != nil {
		_ = waitForDone(context.Background(), dropDone)
	}
	m.finishStreamOperation(key, operationDone)

	m.notifyDownloadsChanged()
	log.Printf("Stream cleaned up: key=%v", key)
}

func waitForDone(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-done:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *manager) reserveStreamOperationLocked(key streamKey) chan struct{} {
	if m.streamOps == nil {
		m.streamOps = make(map[streamKey]chan struct{})
	}
	done := make(chan struct{})
	m.streamOps[key] = done
	return done
}

func (m *manager) finishStreamOperation(key streamKey, done chan struct{}) {
	m.mu.Lock()
	if m.streamOps[key] == done {
		delete(m.streamOps, key)
		close(done)
	}
	m.mu.Unlock()
}

func (m *manager) retainTorrent(ctx context.Context, magnet string) (*torrent.Torrent, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		t, err := addMagnet(m.client, magnet)
		if err != nil {
			return nil, "", err
		}
		hash := t.InfoHash().HexString()

		m.mu.Lock()
		operationDone := m.deletions[hash]
		if operationDone == nil {
			operationDone = m.torrentDrops[hash]
		}
		if operationDone == nil {
			if m.torrents == nil {
				m.torrents = make(map[string]int)
			}
			m.torrents[hash]++
			m.mu.Unlock()
			return t, hash, nil
		}
		m.mu.Unlock()
		if err := waitForDone(ctx, operationDone); err != nil {
			return nil, "", err
		}
	}
}

func (m *manager) reserveDownloadDeletion(ctx context.Context, hash string) (chan struct{}, error) {
	for {
		m.mu.Lock()
		if m.deletions == nil {
			m.deletions = make(map[string]chan struct{})
		}
		if done := m.deletions[hash]; done != nil {
			m.mu.Unlock()
			if err := waitForDone(ctx, done); err != nil {
				return nil, err
			}
			continue
		}
		done := make(chan struct{})
		m.deletions[hash] = done
		m.mu.Unlock()
		return done, nil
	}
}

func (m *manager) finishDownloadDeletion(hash string, done chan struct{}) {
	m.mu.Lock()
	if m.deletions[hash] == done {
		delete(m.deletions, hash)
		close(done)
	}
	m.mu.Unlock()
}

func (m *manager) releaseTorrent(hash string, t *torrent.Torrent) <-chan struct{} {
	m.mu.Lock()
	shouldDrop := m.releaseTorrentLocked(hash)
	if !shouldDrop || t == nil {
		m.mu.Unlock()
		return nil
	}
	done, started := m.reserveTorrentDropLocked(hash)
	m.mu.Unlock()
	if started {
		go m.finishTorrentDrop(hash, t, done)
	}
	return done
}

func (m *manager) dropTorrent(ctx context.Context, hash string, t *torrent.Torrent) error {
	m.mu.Lock()
	if refs := m.torrents[hash]; refs != 0 {
		m.mu.Unlock()
		return fmt.Errorf("torrent %s is still in use", hash)
	}
	done, started := m.reserveTorrentDropLocked(hash)
	m.mu.Unlock()
	if started {
		go m.finishTorrentDrop(hash, t, done)
	}
	return waitForDone(ctx, done)
}

func (m *manager) reserveTorrentDropLocked(hash string) (chan struct{}, bool) {
	if m.torrentDrops == nil {
		m.torrentDrops = make(map[string]chan struct{})
	}
	if done := m.torrentDrops[hash]; done != nil {
		return done, false
	}
	done := make(chan struct{})
	m.torrentDrops[hash] = done
	return done, true
}

func (m *manager) finishTorrentDrop(hash string, t *torrent.Torrent, done chan struct{}) {
	log.Printf("Dropping torrent: %s", hash)
	t.Drop()
	m.mu.Lock()
	if m.torrentDrops[hash] == done {
		delete(m.torrentDrops, hash)
		close(done)
	}
	m.mu.Unlock()
}

func (m *manager) releaseTorrentLocked(hash string) bool {
	cnt, ok := m.torrents[hash]
	if !ok {
		return false
	}
	if cnt > 1 {
		m.torrents[hash] = cnt - 1
		return false
	}
	delete(m.torrents, hash)
	return true
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
	var cleanupDone chan struct{}
	if abandoned {
		cleanedStream, cleanupDone, cleaned = m.cleanupMatchingLocked(key, s, runID, true)
	}
	m.mu.Unlock()
	if cleaned {
		go m.finishStreamCleanup(key, cleanedStream, cleanupDone)
	}
	return cleaned
}

func (m *manager) pauseStream(key streamKey) {
	m.mu.Lock()
	if m.streamOps[key] != nil {
		m.mu.Unlock()
		return
	}
	s, ok := m.active[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	if s.paused || s.completed || !s.running {
		m.mu.Unlock()
		return
	}
	operationDone := m.reserveStreamOperationLocked(key)
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.paused = true
	s.running = false
	file := s.file
	m.mu.Unlock()
	file.SetPriority(torrent.PiecePriorityNone)
	m.finishStreamOperation(key, operationDone)
	m.notifyDownloadsChanged()
	log.Printf("Paused stream due to inactivity: key=%v", key)
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
				go func(key streamKey) {
					if err := m.cleanup(context.Background(), key); err != nil {
						log.Printf("Failed to clean up stream: key=%v, err=%v", key, err)
					}
				}(key)
			}
		}
		m.mu.Unlock()
		m.enforceCacheLimit()
		m.cleanupExpiredDownloads(context.Background())
	}
}
