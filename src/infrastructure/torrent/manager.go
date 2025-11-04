package torrent

import (
	"cinemator/infrastructure/ffmpeg"
	"cinemator/presentation/settings"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cinemator/application"
	"cinemator/domain"

	"github.com/anacrolix/torrent"
)

type manager struct {
	client   *torrent.Client
	active   map[streamKey]*streamInfo
	mu       sync.Mutex
	settings settings.Settings
}

func NewManager(settings settings.Settings) (application.TorrentManager, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = settings.DownloadPath()
	client, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if mkdirErr := os.MkdirAll(settings.HlsPath(), 0755); mkdirErr != nil {
		return nil, mkdirErr
	}
	if mkdirErr := os.MkdirAll(settings.DownloadPath(), 0755); mkdirErr != nil {
		return nil, mkdirErr
	}
	m := &manager{
		client:   client,
		active:   make(map[streamKey]*streamInfo),
		settings: settings,
	}
	go m.viewerWatcher()
	return m, nil
}

func (m *manager) GetTorrentFiles(ctx context.Context, magnet string) ([]domain.FileInfo, error) {
	t, err := m.client.AddMagnet(magnet)
	if err != nil {
		return nil, err
	}
	out := make(chan []domain.FileInfo, 1)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-t.GotInfo():
			files := t.Files()
			result := make([]domain.FileInfo, len(files))
			for i, f := range files {
				result[i] = domain.FileInfo{Index: i, Name: f.DisplayPath(), Size: f.Length()}
			}
			select {
			case out <- result:
			case <-ctx.Done():
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-out:
		return result, nil
	}
}

func (m *manager) PrepareHlsStream(ctx context.Context, magnet string, fileIndex int) (string, string, context.CancelFunc, error) {
	t, err := m.client.AddMagnet(magnet)
	if err != nil {
		log.Printf("PrepareHlsStream: AddMagnet failed: %v", err)
		return "", "", nil, err
	}
	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return "", "", nil, ctx.Err()
	}
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		log.Printf("PrepareHlsStream: bad file index: %d", fileIndex)
		return "", "", nil, fmt.Errorf("bad file index")
	}
	file := files[fileIndex]
	hash := t.InfoHash().HexString()
	key := streamKey{InfoHash: hash, Index: fileIndex}
	outDir := filepath.Join(m.settings.HlsPath(), fmt.Sprintf("%s_%d", hash, fileIndex))
	playlist := filepath.Join(outDir, "index.m3u8")

	m.mu.Lock()
	s, exists := m.active[key]
	if exists {
		s.mtx.Lock()
		s.viewers++
		log.Printf("Client joined existing stream: key=%v, viewers=%d", key, s.viewers)
		s.lastView = time.Now()
		s.mtx.Unlock()
		m.mu.Unlock()
		<-s.ready
		return playlist, outDir, s.cancel, nil
	}
	ready := make(chan struct{})
	streamCtx, cancel := context.WithCancel(context.Background())
	s = &streamInfo{
		ready:    ready,
		cancel:   cancel,
		torrent:  t,
		file:     file,
		viewers:  1,
		lastView: time.Now(),
	}
	m.active[key] = s
	m.mu.Unlock()

	if mkdirErr := os.MkdirAll(outDir, 0755); mkdirErr != nil {
		m.cleanup(key)
		return "", "", nil, fmt.Errorf("mkdir %s: %w", outDir, mkdirErr)
	}
	file.Download()

	// convertFileToStream closes `ready` itself (success or error)
	if probeErr := m.convertFileToStream(streamCtx, file, outDir, playlist, key, ready); probeErr != nil {
		return "", "", nil, probeErr
	}
	log.Printf("Stream ready: key=%v, playlist=%s", key, playlist)
	return playlist, outDir, cancel, nil
}
func (m *manager) convertFileToStream(
	ctx context.Context,
	f *torrent.File,
	outDir, playlist string,
	key streamKey,
	ready chan struct{},
) error {
	// 1) Wait until we have enough bytes for FFMPEG probe
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	minProbeSizeBytes := int64(m.settings.MinProbeSizeMiB()) << 20 // MiB to bytes
	for {
		if f.BytesCompleted() >= minProbeSizeBytes {
			break
		}
		select {
		case <-ctx.Done():
			close(ready)
			return ctx.Err()
		case <-ticker.C:
			// recheck bytes completed
		}
	}
	// 2) Convert torrent into HLS by running ffmpeg process in background (it might block)
	ffmpegHandler := ffmpeg.NewConverter(ctx, func() io.ReadCloser {
		return f.NewReader()
	}, outDir, playlist)
	errCh := make(chan error, 1)
	go func() {
		errCh <- ffmpegHandler.ConvertToHLS()
	}()
	// 3) Wait until playlist appears OR we get an error OR ctx cancelled
	// wait for playlist OR error OR ctx cancel
	playlistReady := make(chan error, 1)
	go func() { playlistReady <- waitForPlaylist(ctx, playlist) }()
	select {
	case <-ctx.Done():
		close(ready)
		return ctx.Err()
	case err := <-errCh:
		close(ready)
		if err != nil {
			m.cleanup(key)
			log.Printf("FFmpeg error: %v", err)
		}
		return err
	case err := <-playlistReady:
		close(ready)
		if err != nil {
			m.cleanup(key)
			log.Printf("waitForPlaylist failed: %v", err)
			return err
		}
		return nil
	}
}

func (m *manager) CleanupStreams() {
	now := time.Now()
	m.mu.Lock()
	for key, s := range m.active {
		s.mtx.Lock()
		noViewers := s.viewers <= 0 || now.Sub(s.lastView) > m.settings.ViewerTimeout()
		s.mtx.Unlock()
		if noViewers {
			go m.cleanup(key)
		}
	}
	m.mu.Unlock()
}
func (m *manager) cleanup(key streamKey) {
	m.mu.Lock()
	s, ok := m.active[key]
	if !ok {
		log.Printf("cleanup called, but no active stream found: key=%v", key)
		m.mu.Unlock()
		return
	}
	s.cancel()
	outDir := filepath.Join(m.settings.HlsPath(), fmt.Sprintf("%s_%d", key.InfoHash, key.Index))
	log.Printf("Cleaning up stream: key=%v, dir=%s", key, outDir)
	err := os.RemoveAll(outDir)
	if err != nil {
		log.Printf("Failed to cleanup directory: %s, err=%v", outDir, err)
	}
	if s.file != nil {
		s.file.SetPriority(0)
	}
	delete(m.active, key)
	m.mu.Unlock()
	log.Printf("Stream cleaned up: key=%v", key)
}

func (m *manager) viewerWatcher() {
	ticker := time.NewTicker(m.settings.ViewerTimeout() / 3)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		m.mu.Lock()
		for key, s := range m.active {
			s.mtx.Lock()
			noViewers := s.viewers <= 0 || now.Sub(s.lastView) > m.settings.ViewerTimeout()
			s.mtx.Unlock()
			if noViewers {
				log.Printf("Viewer timeout detected, cleaning up stream: key=%v", key)
				go m.cleanup(key)
			}
		}
		m.mu.Unlock()
	}
}

// helpers
func waitForPlaylist(ctx context.Context, path string) error {
	const (
		timeout = 5 * time.Minute
		step    = 120 * time.Millisecond
	)
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if _, err := os.Stat(path); err == nil {
				return nil
			}
			if time.Now().After(deadline) {
				log.Printf("waitForPlaylist: %s not found after %v", path, timeout)
				return errors.New("playlist not ready (timeout)")
			}
			time.Sleep(step)
		}
	}
}
