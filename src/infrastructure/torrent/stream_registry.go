package torrent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

func (m *manager) CleanupStreams() {
	now := time.Now()
	m.mu.Lock()
	for key, stream := range m.active {
		if stream.idleAt(now, m.settings.ViewerTimeout()) {
			m.scheduleStreamCleanupLocked(key, stream)
		}
	}
	m.mu.Unlock()
	m.enforceCacheLimit()
	m.cleanupExpiredDownloads(context.Background())
}

func (m *manager) cleanup(key streamKey) {
	m.mu.Lock()
	stream := m.active[key]
	if stream == nil {
		m.mu.Unlock()
		return
	}
	done, started := stream.beginClose()
	m.mu.Unlock()
	if !started {
		<-done
		return
	}
	m.finishStreamCleanup(key, stream)
	m.unregisterCleanedStream(key, stream)
}

func (m *manager) shutdownStreams(ctx context.Context) error {
	m.mu.Lock()
	keys := make([]streamKey, 0, len(m.active))
	for key := range m.active {
		keys = append(keys, key)
	}
	m.mu.Unlock()

	var current sync.WaitGroup
	for _, key := range keys {
		current.Add(1)
		go func() {
			defer current.Done()
			m.cleanup(key)
		}()
	}
	done := make(chan struct{})
	go func() {
		current.Wait()
		m.cleanupWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop stream workers: %w", ctx.Err())
	}
}

// reuseOrRetireStreamLocked returns only a stream that can still accept work.
// A closing or failed stream keeps ownership of its paths until cleanup ends,
// so callers must wait for cleanupDone before attempting to publish a successor.
func (m *manager) reuseOrRetireStreamLocked(key streamKey, now time.Time) (*streamInfo, <-chan struct{}) {
	stream := m.active[key]
	if stream == nil {
		return nil, nil
	}
	stream.mtx.Lock()
	reusable := !stream.closing && stream.fatalErr == nil
	if reusable {
		stream.lastView = now
	}
	stream.mtx.Unlock()
	if reusable {
		return stream, nil
	}
	return nil, m.scheduleStreamCleanupLocked(key, stream)
}

func (m *manager) scheduleStreamCleanupLocked(key streamKey, stream *streamInfo) <-chan struct{} {
	done, started := stream.beginClose()
	if !started {
		return done
	}
	m.cleanupWG.Add(1)
	go func() {
		defer m.cleanupWG.Done()
		m.finishStreamCleanup(key, stream)
		m.unregisterCleanedStream(key, stream)
	}()
	return done
}

func waitForStreamCleanup(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *manager) finishStreamCleanup(key streamKey, stream *streamInfo) {
	waitForStreamWorkers(stream)
	stream.source.Close()
	log.Printf("Cleaning up stream: key=%v, dir=%s", key, stream.paths.outDir)
	stream.generationMtx.Lock()
	stream.playlistMtx.Lock()
	if err := m.media.retireHls(stream.paths.outDir); err != nil {
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
	if m.active[key] == stream {
		delete(m.active, key)
	}
	m.mu.Unlock()
	m.releaseTorrentUse(key.InfoHash, true)
	close(stream.cleanupDone)
}

func waitForStreamWorkers(stream *streamInfo) {
	if stream.ready != nil {
		<-stream.ready
	}
	for _, job := range stream.activeJobs() {
		<-job.done
	}
}

func (m *manager) resetStreamOutput(paths streamPaths) error {
	return m.media.resetHls(paths.outDir)
}

func (m *manager) TouchStream(_ context.Context, dirName string) {
	key, err := parseStreamDir(dirName)
	if err != nil {
		return
	}
	m.mu.Lock()
	if stream, ok := m.active[key]; ok {
		stream.touch(time.Now())
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
