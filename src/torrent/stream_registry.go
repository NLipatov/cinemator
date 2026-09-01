package torrent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

const (
	webseedStopTimeout  = 5 * time.Second
	webseedPollInterval = 100 * time.Millisecond
	streamReadyVersion  = "2\n"
)

func (m *Manager) cleanup(ctx context.Context, key streamKey) error {
	return m.cleanupMatching(ctx, key, nil)
}

func (m *Manager) cleanupIfCurrent(key streamKey, expected *streamInfo) {
	if err := m.cleanupMatching(context.Background(), key, expected); err != nil {
		log.Printf("Failed to clean up stream: key=%v, err=%v", key, err)
	}
}

func (m *Manager) cleanupMatching(ctx context.Context, key streamKey, expected *streamInfo) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		m.mu.Lock()
		s, operationDone, started := m.cleanupMatchingLocked(key, expected)
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

func (m *Manager) cleanupMatchingLocked(key streamKey, expected *streamInfo) (*streamInfo, chan struct{}, bool) {
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
	if s.cancel != nil {
		s.cancel()
	}
	operationDone := m.reserveStreamOperationLocked(key)
	delete(m.active, key)
	return s, operationDone, true
}

func (m *Manager) finishStreamCleanup(key streamKey, s *streamInfo, operationDone chan struct{}) {
	if s.source != nil {
		s.source.Close()
	}
	if s.runDone != nil {
		<-s.runDone
	}
	log.Printf("Cleaning up stream: key=%v, dir=%s", key, s.paths.outDir)
	ready, err := streamOutputReady(s.paths)
	if err != nil {
		log.Printf("Failed to inspect stream output: %s, err=%v", s.paths.outDir, err)
	}
	if s.completed && ready {
		s.mtx.Lock()
		lastView := s.lastView
		s.mtx.Unlock()
		if err := os.Chtimes(s.paths.outDir, lastView, lastView); err != nil {
			log.Printf("Failed to update HLS cache access time: %s, err=%v", s.paths.outDir, err)
		}
	} else if err := os.RemoveAll(s.paths.outDir); err != nil {
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

func (m *Manager) reserveStreamOperationLocked(key streamKey) chan struct{} {
	if m.streamOps == nil {
		m.streamOps = make(map[streamKey]chan struct{})
	}
	done := make(chan struct{})
	m.streamOps[key] = done
	return done
}

func (m *Manager) finishStreamOperation(key streamKey, done chan struct{}) {
	m.mu.Lock()
	if m.streamOps[key] == done {
		delete(m.streamOps, key)
		close(done)
	}
	m.mu.Unlock()
}

func (m *Manager) retainTorrent(ctx context.Context, magnet string) (*torrent.Torrent, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	spec, hash, err := parseMagnet(magnet)
	if err != nil {
		return nil, "", err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}

		operationDone, err := m.reserveTorrentOperation(ctx, hash)
		if err != nil {
			return nil, "", err
		}

		m.mu.Lock()
		deletionDone := m.deletions[hash]
		m.mu.Unlock()
		if deletionDone != nil {
			m.finishTorrentOperation(hash, operationDone)
			if err := waitForDone(ctx, deletionDone); err != nil {
				return nil, "", err
			}
			continue
		}

		t, _, err := m.client.AddTorrentSpec(spec)
		if err != nil {
			m.finishTorrentOperation(hash, operationDone)
			return nil, "", err
		}
		if t == nil {
			m.finishTorrentOperation(hash, operationDone)
			return nil, "", fmt.Errorf("torrent not created")
		}

		// A deletion reserved while AddTorrentSpec was running wins this race.
		m.mu.Lock()
		deletionDone = m.deletions[hash]
		if deletionDone == nil {
			if m.torrents == nil {
				m.torrents = make(map[string]int)
			}
			m.torrents[hash]++
		}
		m.mu.Unlock()
		m.finishTorrentOperation(hash, operationDone)
		if deletionDone == nil {
			return t, hash, nil
		}
		if err := waitForDone(ctx, deletionDone); err != nil {
			return nil, "", err
		}
	}
}

func (m *Manager) reserveTorrentOperation(ctx context.Context, hash string) (chan struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.mu.Lock()
		if m.torrentOps == nil {
			m.torrentOps = make(map[string]chan struct{})
		}
		if done := m.torrentOps[hash]; done != nil {
			m.mu.Unlock()
			if err := waitForDone(ctx, done); err != nil {
				return nil, err
			}
			continue
		}
		done := make(chan struct{})
		m.torrentOps[hash] = done
		m.mu.Unlock()
		return done, nil
	}
}

func (m *Manager) finishTorrentOperation(hash string, done chan struct{}) {
	m.mu.Lock()
	if m.torrentOps[hash] == done {
		delete(m.torrentOps, hash)
		close(done)
	}
	m.mu.Unlock()
}

func (m *Manager) reserveDownloadDeletion(ctx context.Context, hash string) (chan struct{}, error) {
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

func (m *Manager) finishDownloadDeletion(hash string, done chan struct{}) {
	m.mu.Lock()
	if m.deletions[hash] == done {
		delete(m.deletions, hash)
		close(done)
	}
	m.mu.Unlock()
}

func (m *Manager) releaseTorrent(hash string, t *torrent.Torrent) <-chan struct{} {
	if t == nil {
		return nil
	}
	m.mu.Lock()
	shouldDrop := m.releaseTorrentLocked(hash)
	if !shouldDrop {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		operationDone, _ := m.reserveTorrentOperation(context.Background(), hash)
		m.mu.Lock()
		refs := m.torrents[hash]
		m.mu.Unlock()
		current, exists := m.client.Torrent(t.InfoHash())
		payloadCanBeDeleted := !exists
		if refs == 0 && exists && current == t {
			log.Printf("Dropping torrent: %s", hash)
			dropCtx, cancel := context.WithTimeout(context.Background(), webseedStopTimeout)
			err := m.dropTorrentAfterWebseedsStop(dropCtx, t)
			cancel()
			if err != nil {
				log.Printf("Failed to drop torrent %s: %v", hash, err)
			} else {
				payloadCanBeDeleted = true
			}
		}
		if refs == 0 && payloadCanBeDeleted && m.hasReadyHLS(hash) && m.downloads != nil {
			if err := m.downloads.deletePayload(context.Background(), hash); err != nil {
				log.Printf("Failed to delete completed torrent payload: %s, err=%v", hash, err)
			} else {
				m.notifyDownloadsChanged()
			}
		}
		m.finishTorrentOperation(hash, operationDone)
		close(done)
	}()
	return done
}

func (m *Manager) dropTorrent(ctx context.Context, hash string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	operationDone, err := m.reserveTorrentOperation(ctx, hash)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		m.finishTorrentOperation(hash, operationDone)
		return err
	}
	m.mu.Lock()
	if refs := m.torrents[hash]; refs != 0 {
		m.mu.Unlock()
		m.finishTorrentOperation(hash, operationDone)
		return fmt.Errorf("torrent %s is still in use", hash)
	}
	m.mu.Unlock()

	t, ok := m.client.Torrent(metainfo.NewHashFromHex(hash))
	if !ok {
		m.finishTorrentOperation(hash, operationDone)
		return nil
	}

	dropResult := make(chan error, 1)
	go func() {
		log.Printf("Dropping torrent: %s", hash)
		dropCtx, cancel := context.WithTimeout(ctx, webseedStopTimeout)
		err := m.dropTorrentAfterWebseedsStop(dropCtx, t)
		cancel()
		m.finishTorrentOperation(hash, operationDone)
		dropResult <- err
	}()
	select {
	case err := <-dropResult:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) dropTorrentAfterWebseedsStop(ctx context.Context, t *torrent.Torrent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// anacrolix removes webseed requests asynchronously. Dropping the torrent
	// before that cleanup finishes can trip its client-wide consistency check.
	t.DisallowDataDownload()
	dropped := false
	defer func() {
		if !dropped {
			t.AllowDataDownload()
		}
	}()
	ticker := time.NewTicker(webseedPollInterval)
	defer ticker.Stop()
	for {
		var status strings.Builder
		m.client.WriteStatus(&status)
		if !torrentStatusHasActiveWebseedRequests(status.String(), t.InfoHash().HexString()) {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for webseed requests to stop: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	t.Drop()
	dropped = true
	return nil
}

func torrentStatusHasActiveWebseedRequests(status, hash string) bool {
	currentHash := ""
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "Infohash: ") {
			currentHash = strings.TrimSpace(strings.TrimPrefix(line, "Infohash: "))
			continue
		}
		if strings.EqualFold(currentHash, hash) && strings.Contains(line, "active requests:") {
			return true
		}
	}
	return false
}

func (m *Manager) releaseTorrentLocked(hash string) bool {
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

func resetStreamOutput(paths streamPaths) error {
	if err := os.RemoveAll(paths.outDir); err != nil {
		return err
	}
	return os.MkdirAll(paths.outDir, 0755)
}

func markStreamOutputReady(paths streamPaths) error {
	tmp := paths.readyMarker + ".tmp"
	if err := os.WriteFile(tmp, []byte(streamReadyVersion), 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, paths.readyMarker); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func streamOutputReady(paths streamPaths) (bool, error) {
	marker, err := os.ReadFile(paths.readyMarker)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if string(marker) != streamReadyVersion {
		return false, nil
	}
	info, err := os.Stat(paths.masterPlaylist)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return !info.IsDir(), nil
}

func (m *Manager) hasReadyHLS(hash string) bool {
	entries, err := os.ReadDir(m.cfg.HLSPath)
	if err != nil {
		return false
	}
	prefix := hash + "_"
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		key, err := parseStreamDir(entry.Name())
		if err != nil {
			continue
		}
		ready, err := streamOutputReady(key.paths(m.cfg.HLSPath))
		if err == nil && ready {
			return true
		}
	}
	return false
}

func (m *Manager) TouchStream(_ context.Context, dirName string) {
	key, err := parseStreamDir(dirName)
	if err != nil {
		return
	}
	m.mu.Lock()
	if s, ok := m.active[key]; ok {
		s.mtx.Lock()
		s.lastView = time.Now()
		s.mtx.Unlock()
	}
	m.mu.Unlock()
}

func (m *Manager) cleanupInactiveCompletedStreams(now time.Time) {
	type inactiveStream struct {
		key    streamKey
		stream *streamInfo
	}

	m.mu.Lock()
	var inactive []inactiveStream
	for key, s := range m.active {
		s.mtx.Lock()
		shouldCleanup := s.completed && now.Sub(s.lastView) > m.cfg.ViewerTimeout
		s.mtx.Unlock()
		if shouldCleanup {
			inactive = append(inactive, inactiveStream{key: key, stream: s})
		}
	}
	m.mu.Unlock()

	for _, item := range inactive {
		log.Printf("Viewer timeout detected, deactivating completed stream: key=%v", item.key)
		if err := m.cleanupMatching(context.Background(), item.key, item.stream); err != nil {
			log.Printf("Failed to clean up completed stream: key=%v, err=%v", item.key, err)
		}
	}
}

func (m *Manager) viewerWatcher() {
	ticker := time.NewTicker(time.Minute / 3)
	defer ticker.Stop()
	for range ticker.C {
		m.cleanupInactiveCompletedStreams(time.Now())
		m.enforceCacheLimit()
		m.cleanupExpiredDownloads(context.Background())
	}
}
