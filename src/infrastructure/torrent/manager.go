package torrent

import (
	"cinemator/presentation/settings"
	"context"
	"fmt"
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
	_ = os.MkdirAll(settings.HlsPath(), 0755)
	_ = os.MkdirAll(settings.DownloadPath(), 0755)
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

	done := make(chan []domain.FileInfo, 1)
	go func() {
		<-t.GotInfo()
		files := t.Files()
		out := make([]domain.FileInfo, len(files))
		for i, f := range files {
			out[i] = domain.FileInfo{Index: i, Name: f.DisplayPath(), Size: f.Length()}
		}

		// prevents the goroutine from hanging if ctx is cancelled
		select {
		case done <- out:
		case <-ctx.Done():
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-done:
		return res, nil
	}
}

func (m *manager) PrepareHlsStream(ctx context.Context, magnet string, fileIndex int) (*torrent.File, string, string, context.CancelFunc, error) {
	t, err := m.client.AddMagnet(magnet)
	if err != nil {
		log.Printf("PrepareHlsStream: AddMagnet failed: %v", err)
		return nil, "", "", nil, err
	}
	<-t.GotInfo()
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		log.Printf("PrepareHlsStream: bad file index: %d", fileIndex)
		return nil, "", "", nil, fmt.Errorf("bad file index")
	}
	f := files[fileIndex]
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
		return nil, playlist, outDir, s.cancel, nil
	}
	log.Printf("Starting new stream: key=%v", key)
	m.mu.Unlock()

	_ = os.MkdirAll(outDir, 0755)
	f.Download()
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)
	s = &streamInfo{
		ready:    ready,
		cancel:   cancel,
		torrent:  t,
		file:     f,
		viewers:  1,
		lastView: time.Now(),
	}
	m.mu.Lock()
	m.active[key] = s
	m.mu.Unlock()
	log.Printf("Stream ready: key=%v, playlist=%s", key, playlist)
	return f, playlist, outDir, cancel, nil
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
