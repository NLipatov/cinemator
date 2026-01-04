package torrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"cinemator/application"
	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
	"cinemator/presentation/settings"

	"github.com/anacrolix/torrent"
)

type manager struct {
	client   *torrent.Client
	active   map[streamKey]*streamInfo
	torrents map[string]int
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
		torrents: make(map[string]int),
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

func (m *manager) GetMediaInfo(ctx context.Context, magnet string, fileIndex int) (domain.MediaInfo, error) {
	t, err := m.client.AddMagnet(magnet)
	if err != nil {
		return domain.MediaInfo{}, err
	}
	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return domain.MediaInfo{}, ctx.Err()
	}
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return domain.MediaInfo{}, fmt.Errorf("bad file index")
	}
	file := files[fileIndex]
	origPrio := file.Priority()
	file.SetPriority(torrent.PiecePriorityHigh)
	file.Download()

	// Wait for enough bytes to probe
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	minProbeSizeBytes := int64(m.settings.MinProbeSizeMiB()) << 20
	for {
		if file.BytesCompleted() >= minProbeSizeBytes {
			break
		}
		select {
		case <-ctx.Done():
			return domain.MediaInfo{}, ctx.Err()
		case <-ticker.C:
		}
	}

	analyzer := ffmpeg.SampleAnalyzer{}
	reader := file.NewReader()
	defer reader.Close()
	info, analyzeErr := analyzer.Analyze(reader)
	file.SetPriority(origPrio)
	return info, analyzeErr
}

func (m *manager) PrepareHlsStream(ctx context.Context, magnet string, fileIndex, audioTrack, subtitleTrack int) (string, string, context.CancelFunc, error) {
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
	key := streamKey{InfoHash: hash, Index: fileIndex, Audio: audioTrack, Subtitle: subtitleTrack}
	outDir := filepath.Join(m.settings.HlsPath(), key.dirName())
	videoPlaylist := filepath.Join(outDir, "index.m3u8")
	subtitlePlaylist := filepath.Join(outDir, "subs.m3u8")
	masterPlaylist := filepath.Join(outDir, "master.m3u8")

	m.mu.Lock()
	s, exists := m.active[key]
	if exists {
		s.mtx.Lock()
		s.lastView = time.Now()
		s.mtx.Unlock()
		m.mu.Unlock()
		<-s.ready
		return masterPlaylist, outDir, s.cancel, nil
	}
	ready := make(chan struct{})
	streamCtx, cancel := context.WithCancel(context.Background())
	s = &streamInfo{
		ready:            ready,
		cancel:           cancel,
		torrent:          t,
		file:             file,
		lastView:         time.Now(),
		outDir:           outDir,
		selection:        ffmpeg.StreamSelection{AudioTrackIndex: audioTrack, SubtitleTrackIndex: subtitleTrack},
		videoPlaylist:    videoPlaylist,
		subtitlePlaylist: subtitlePlaylist,
		masterPlaylist:   masterPlaylist,
		running:          true,
	}
	m.active[key] = s
	m.torrents[hash]++
	m.mu.Unlock()

	if mkdirErr := os.MkdirAll(outDir, 0755); mkdirErr != nil {
		m.cleanup(key)
		return "", "", nil, fmt.Errorf("mkdir %s: %w", outDir, mkdirErr)
	}
	file.Download()
	file.SetPriority(torrent.PiecePriorityHigh)
	m.preloadLeadingPieces(file)

	// convertFileToStream closes `ready` itself (success or error)
	if probeErr := m.convertFileToStream(streamCtx, streamCtx, file, outDir, videoPlaylist, subtitlePlaylist, masterPlaylist, key, ready, s.selection); probeErr != nil {
		m.cleanup(key)
		return "", "", nil, probeErr
	}
	log.Printf("Stream ready: key=%v, playlist=%s", key, masterPlaylist)
	return masterPlaylist, outDir, cancel, nil
}

func (m *manager) preloadLeadingPieces(f *torrent.File) {
	const preloadPieces = 8
	begin := f.BeginPieceIndex()
	end := begin + preloadPieces
	if end > f.EndPieceIndex() {
		end = f.EndPieceIndex()
	}
	if end > begin {
		f.Torrent().DownloadPieces(begin, end)
	}
}
func (m *manager) convertFileToStream(
	streamCtx context.Context,
	prepareCtx context.Context,
	f *torrent.File,
	outDir, playlist string,
	subtitlePlaylist string,
	masterPlaylist string,
	key streamKey,
	ready chan struct{},
	selection ffmpeg.StreamSelection,
) error {
	if prepareCtx == nil {
		prepareCtx = context.Background()
	}
	// 1) Wait until we have enough bytes for FFMPEG probe
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	minProbeSizeBytes := int64(m.settings.MinProbeSizeMiB()) << 20 // MiB to bytes
	for {
		if f.BytesCompleted() >= minProbeSizeBytes {
			break
		}
		select {
		case <-prepareCtx.Done():
			close(ready)
			return prepareCtx.Err()
		case <-ticker.C:
			// recheck bytes completed
		}
	}
	// 2) Convert torrent into HLS by running ffmpeg process in background (it might block)
	ffmpegHandler := ffmpeg.NewConverter(streamCtx, func() io.ReadCloser {
		reader := f.NewReader()
		reader.SetContext(streamCtx)
		reader.SetReadahead(32 << 20) // 32 MiB for faster sequential fetch
		reader.SetResponsive()
		return reader
	}, outDir, playlist, subtitlePlaylist, masterPlaylist, selection)
	errCh := make(chan error, 1)
	go func() {
		errCh <- ffmpegHandler.ConvertToHLS()
	}()
	// 3) Wait until playlist appears OR we get an error OR ctx cancelled
	// wait for playlist OR error OR ctx cancel
	playlistReady := make(chan error, 1)
	go func() { playlistReady <- waitForPlaylist(prepareCtx, masterPlaylist) }()
	select {
	case <-prepareCtx.Done():
		close(ready)
		return prepareCtx.Err()
	case err := <-errCh:
		close(ready)
		if err != nil && !errors.Is(err, context.Canceled) {
			m.cleanup(key)
			log.Printf("FFmpeg error: %v", err)
		}
		return err
	case err := <-playlistReady:
		close(ready)
		if err != nil && !errors.Is(err, context.Canceled) {
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
	m.mu.Lock()
	s, ok := m.active[key]
	if !ok {
		log.Printf("cleanup called, but no active stream found: key=%v", key)
		m.mu.Unlock()
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	outDir := filepath.Join(m.settings.HlsPath(), key.dirName())
	shouldDrop := false
	if cnt, ok := m.torrents[key.InfoHash]; ok {
		if cnt <= 1 {
			delete(m.torrents, key.InfoHash)
			shouldDrop = true
		} else {
			m.torrents[key.InfoHash] = cnt - 1
		}
	}
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
	if shouldDrop {
		log.Printf("Dropping torrent: %s", key.InfoHash)
		s.torrent.Drop()
		dlDir := filepath.Join(m.settings.DownloadPath(), key.InfoHash)
		if rmErr := os.RemoveAll(dlDir); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Printf("Failed to remove download dir %s: %v", dlDir, rmErr)
		}
	}
}

func (m *manager) enforceCacheLimit() {
	limit := m.settings.MaxCacheBytes()
	if limit <= 0 {
		return
	}
	type item struct {
		path   string
		size   int64
		last   time.Time
		active bool
		key    streamKey
	}
	var (
		total int64
		items []item
	)
	root := m.settings.HlsPath()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		key, err := parseStreamDir(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(root, e.Name())
		var size int64
		filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if info, err := d.Info(); err == nil && !info.IsDir() {
				size += info.Size()
			}
			return nil
		})
		total += size
		it := item{path: dir, size: size, last: time.Now(), key: key}
		if s, ok := m.active[key]; ok {
			it.last = s.lastView
			it.active = true
		} else if info, err := os.Stat(dir); err == nil {
			it.last = info.ModTime()
		}
		items = append(items, it)
	}

	if total <= limit {
		return
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].last.Before(items[j].last)
	})

	for _, it := range items {
		if total <= limit {
			break
		}
		if it.active {
			continue
		}
		if err := os.RemoveAll(it.path); err != nil {
			log.Printf("enforceCacheLimit: failed to remove %s: %v", it.path, err)
			continue
		}
		total -= it.size
		log.Printf("enforceCacheLimit: removed %s (freed %d bytes)", it.path, it.size)
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
	streamCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.paused = false
	s.running = true
	s.file.SetPriority(torrent.PiecePriorityHigh)
	go func() {
		ready := make(chan struct{})
		s.ready = ready
		err := m.convertFileToStream(streamCtx, streamCtx, s.file, s.outDir, s.videoPlaylist, s.subtitlePlaylist, s.masterPlaylist, key, ready, s.selection)
		if err != nil {
			log.Printf("Stream conversion error (resume): %v", err)
		}
		m.mu.Lock()
		s.running = false
		if errors.Is(err, context.Canceled) {
			s.paused = true
		}
		m.mu.Unlock()
	}()
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
			idle := now.Sub(s.lastView) > time.Minute
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

// helpers
func waitForPlaylist(ctx context.Context, path string) error {
	const (
		timeout = 20 * time.Minute
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
