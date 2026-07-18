package torrent

import (
	"context"
	"errors"
	"log"
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
			m.cleanupWG.Add(1)
			go func() {
				defer m.cleanupWG.Done()
				m.cleanup(key)
			}()
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
	stream, done, clean := m.beginStreamCleanupLocked(key, expected)
	m.mu.Unlock()
	if !clean {
		if done != nil {
			<-done
		}
		return
	}
	m.finishStreamCleanup(key, stream)
	m.unregisterCleanedStream(key, stream)
}

func (m *manager) beginStreamCleanupLocked(key streamKey, expected *streamInfo) (*streamInfo, <-chan struct{}, bool) {
	stream, ok := m.active[key]
	if !ok || expected != nil && stream != expected {
		return nil, nil, false
	}
	stream.mtx.Lock()
	if stream.closing {
		done := stream.cleanupDone
		stream.mtx.Unlock()
		return stream, done, false
	}
	if stream.cleanupDone == nil {
		stream.cleanupDone = make(chan struct{})
	}
	stream.closing = true
	if stream.cancel != nil {
		stream.cancel()
	}
	stream.mtx.Unlock()
	return stream, stream.cleanupDone, true
}

func (m *manager) finishStreamCleanup(key streamKey, stream *streamInfo) {
	waitForStreamWorkers(stream)
	stream.source.Close()
	log.Printf("Cleaning up stream: key=%v, dir=%s", key, stream.paths.outDir)
	stream.generationMtx.Lock()
	stream.playlistMtx.Lock()
	if err := m.assets.RetireTree(stream.paths.outDir); err != nil && !errors.Is(err, errHlsAssetsBusy) {
		log.Printf("Failed to cleanup directory: %s, err=%v", stream.paths.outDir, err)
	}
	stream.playlistMtx.Unlock()
	stream.generationMtx.Unlock()
	if stream.file != nil {
		stream.file.SetPriority(torrent.PiecePriorityNone)
	}
	m.notifyDownloadsChanged()
	log.Printf("Stream cleaned up: key=%v", key)
}

func (m *manager) unregisterCleanedStream(key streamKey, stream *streamInfo) {
	m.mu.Lock()
	if m.active[key] != stream {
		m.mu.Unlock()
		close(stream.cleanupDone)
		return
	}
	delete(m.active, key)
	m.mu.Unlock()
	close(stream.cleanupDone)
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

func (m *manager) resetStreamOutput(paths streamPaths) error {
	return m.assets.ResetTree(paths.outDir)
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
	defer close(m.watcherDone)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.CleanupStreams()
		case <-m.watcherStop:
			return
		}
	}
}
