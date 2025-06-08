package torrent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"cinemator/application"
	"cinemator/domain"

	"github.com/anacrolix/torrent"
)

const (
	hlsPath       = "/tmp/cinemator/hls"
	downloadPath  = "/tmp/cinemator/downloads"
	viewerTimeout = 5 * time.Hour
)

type streamKey struct {
	InfoHash string
	Index    int
}

type streamInfo struct {
	ready    chan struct{}
	cancel   context.CancelFunc
	torrent  *torrent.Torrent
	file     *torrent.File
	viewers  int
	lastView time.Time
	mtx      sync.Mutex
}

type anacrolixManager struct {
	client *torrent.Client
	active map[streamKey]*streamInfo
	mu     sync.Mutex
}

func NewManager() (application.TorrentManager, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = downloadPath
	client, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	_ = os.MkdirAll(hlsPath, 0755)
	_ = os.MkdirAll(downloadPath, 0755)
	m := &anacrolixManager{
		client: client,
		active: make(map[streamKey]*streamInfo),
	}
	go m.viewerWatcher()
	return m, nil
}

func (m *anacrolixManager) Files(ctx context.Context, magnet string) ([]domain.FileInfo, error) {
	t, err := m.client.AddMagnet(magnet)
	if err != nil {
		return nil, err
	}
	<-t.GotInfo()
	files := t.Files()
	result := make([]domain.FileInfo, len(files))
	for i, f := range files {
		result[i] = domain.FileInfo{
			Index: i,
			Name:  f.DisplayPath(),
			Size:  f.Length(),
		}
	}
	return result, nil
}

func (m *anacrolixManager) StartStream(ctx context.Context, magnet string, fileIndex int) (string, string, context.CancelFunc, error) {
	t, err := m.client.AddMagnet(magnet)
	if err != nil {
		return "", "", nil, err
	}
	<-t.GotInfo()
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return "", "", nil, fmt.Errorf("bad file index")
	}
	f := files[fileIndex]
	hash := t.InfoHash().HexString()
	key := streamKey{InfoHash: hash, Index: fileIndex}
	outDir := filepath.Join(hlsPath, fmt.Sprintf("%s_%d", hash, fileIndex))
	playlist := filepath.Join(outDir, "index.m3u8")

	m.mu.Lock()
	s, exists := m.active[key]
	if exists {
		s.mtx.Lock()
		s.viewers++
		s.lastView = time.Now()
		s.mtx.Unlock()
		m.mu.Unlock()
		<-s.ready
		return playlist, outDir, s.cancel, nil
	}
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

	go func() {
		defer close(ready)
		err := runFFmpeg(ctx, f, playlist, outDir)
		if err != nil {
			m.cleanup(key)
		}
	}()

	err = waitForPlaylist(ctx, playlist)
	if err != nil {
		return "", "", nil, fmt.Errorf("playlist not ready: %w", err)
	}
	return playlist, outDir, cancel, nil
}

func (m *anacrolixManager) TouchStream(magnet string, fileIndex int) {
	t, _ := m.client.AddMagnet(magnet)
	hash := t.InfoHash().HexString()
	key := streamKey{InfoHash: hash, Index: fileIndex}
	m.mu.Lock()
	s := m.active[key]
	m.mu.Unlock()
	if s != nil {
		s.mtx.Lock()
		s.lastView = time.Now()
		if s.viewers == 0 {
			s.viewers = 1
		}
		s.mtx.Unlock()
	}
}

func (m *anacrolixManager) CleanupStreams() {
	now := time.Now()
	m.mu.Lock()
	for key, s := range m.active {
		s.mtx.Lock()
		noViewers := s.viewers <= 0 || now.Sub(s.lastView) > viewerTimeout
		s.mtx.Unlock()
		if noViewers {
			go m.cleanup(key)
		}
	}
	m.mu.Unlock()
}

// private methods

func (m *anacrolixManager) cleanup(key streamKey) {
	m.mu.Lock()
	s, ok := m.active[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	s.cancel()
	outDir := filepath.Join(hlsPath, fmt.Sprintf("%s_%d", key.InfoHash, key.Index))
	_ = os.RemoveAll(outDir)
	if s.file != nil {
		s.file.SetPriority(0)
	}
	stillUsed := false
	for k, v := range m.active {
		if k != key && v.torrent == s.torrent {
			stillUsed = true
			break
		}
	}
	if !stillUsed && s.torrent != nil {
		s.torrent.Drop()
	}
	delete(m.active, key)
	m.mu.Unlock()
}

func (m *anacrolixManager) viewerWatcher() {
	ticker := time.NewTicker(viewerTimeout / 3)
	defer ticker.Stop()
	for range ticker.C {
		m.CleanupStreams()
	}
}

// helpers

func runFFmpeg(ctx context.Context, file *torrent.File, playlist, outDir string) error {
	r := file.NewReader()
	defer r.Close()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-fflags", "+genpts",
		"-i", "pipe:0",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-vf", "format=yuv420p",
		"-c:a", "aac", "-b:a", "128k",
		"-ac", "2",
		"-movflags", "+faststart",
		"-f", "hls",
		"-hls_time", "10",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(outDir, "chunk_%05d.ts"),
		playlist,
	)
	cmd.Stdin = r
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

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
