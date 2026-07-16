package torrent

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/anacrolix/torrent"
)

func (m *manager) CleanupStreams() {
	now := time.Now()
	m.mu.Lock()
	for key, stream := range m.active {
		stream.mtx.Lock()
		noViewers := now.Sub(stream.lastView) > m.settings.ViewerTimeout()
		stream.mtx.Unlock()
		if noViewers {
			go m.cleanup(key)
		}
	}
	m.mu.Unlock()
	m.enforceCacheLimit()
	m.cleanupExpiredDownloads(context.Background())
}

func (m *manager) cleanup(key streamKey) {
	m.cleanupMatching(key, nil)
}

func (m *manager) cleanupIfCurrent(key streamKey, expected *streamInfo) {
	m.cleanupMatching(key, expected)
}

func (m *manager) cleanupMatching(key streamKey, expected *streamInfo) {
	m.mu.Lock()
	stream, shouldDrop, cleaned := m.cleanupMatchingLocked(key, expected)
	m.mu.Unlock()
	if cleaned {
		m.finishStreamCleanup(key, stream, shouldDrop)
	}
}

func (m *manager) cleanupMatchingLocked(key streamKey, expected *streamInfo) (*streamInfo, bool, bool) {
	stream, ok := m.active[key]
	if !ok || expected != nil && stream != expected {
		return nil, false, false
	}
	stream.mtx.Lock()
	stream.closing = true
	if stream.cancel != nil {
		stream.cancel()
	}
	stream.mtx.Unlock()
	shouldDrop := false
	if count := m.torrents[key.InfoHash]; count <= 1 {
		delete(m.torrents, key.InfoHash)
		shouldDrop = true
	} else {
		m.torrents[key.InfoHash] = count - 1
	}
	delete(m.active, key)
	return stream, shouldDrop, true
}

func (m *manager) finishStreamCleanup(key streamKey, stream *streamInfo, shouldDrop bool) {
	waitForStreamWorkers(stream)
	stream.source.Close()
	log.Printf("Cleaning up stream: key=%v, dir=%s", key, stream.paths.outDir)
	if err := os.RemoveAll(stream.paths.outDir); err != nil {
		log.Printf("Failed to cleanup directory: %s, err=%v", stream.paths.outDir, err)
	}
	if stream.file != nil {
		stream.file.SetPriority(torrent.PiecePriorityNone)
	}
	m.notifyDownloadsChanged()
	log.Printf("Stream cleaned up: key=%v", key)
	if shouldDrop && stream.torrent != nil {
		log.Printf("Dropping torrent: %s", key.InfoHash)
		stream.torrent.Drop()
	}
}

func waitForStreamWorkers(stream *streamInfo) {
	if stream.ready != nil {
		<-stream.ready
	}
	stream.mtx.Lock()
	jobs := make([]*segmentJob, 0, len(stream.videoJobs)+len(stream.subtitleJobs))
	for job := range stream.videoJobs {
		jobs = append(jobs, job)
	}
	for job := range stream.subtitleJobs {
		jobs = append(jobs, job)
	}
	stream.mtx.Unlock()
	for _, job := range jobs {
		<-job.done
	}
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
	if stream, ok := m.active[key]; ok {
		stream.mtx.Lock()
		stream.lastView = time.Now()
		stream.mtx.Unlock()
	}
	m.mu.Unlock()
}

func (m *manager) viewerWatcher() {
	ticker := time.NewTicker(time.Minute / 3)
	defer ticker.Stop()
	for range ticker.C {
		m.CleanupStreams()
	}
}
