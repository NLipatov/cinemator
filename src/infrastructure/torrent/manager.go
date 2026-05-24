package torrent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
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
	sources  *rangeServer
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
	sources, err := newRangeServer()
	if err != nil {
		return nil, err
	}
	m := &manager{
		client:   client,
		active:   make(map[streamKey]*streamInfo),
		torrents: make(map[string]int),
		sources:  sources,
		settings: settings,
	}
	go m.viewerWatcher()
	return m, nil
}

func (m *manager) GetTorrentFiles(ctx context.Context, magnet string) ([]domain.FileInfo, error) {
	t, err := addMagnet(m.client, magnet)
	if err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.GotInfo():
	}

	files := t.Files()
	result := make([]domain.FileInfo, len(files))
	for i, f := range files {
		result[i] = domain.FileInfo{Index: i, Name: f.DisplayPath(), Size: f.Length()}
	}
	return result, nil
}

func (m *manager) GetMediaInfo(ctx context.Context, magnet string, fileIndex int) (domain.MediaInfo, error) {
	t, err := addMagnet(m.client, magnet)
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
	defer file.SetPriority(origPrio)
	file.Download()

	source, err := newTorrentSource(file, m.sources)
	if err != nil {
		return domain.MediaInfo{}, err
	}
	defer source.Close()
	return source.Probe(ctx)
}

func (m *manager) PrepareHlsStream(ctx context.Context, magnet string, fileIndex, audioTrack, subtitleTrack int) (string, string, context.CancelFunc, error) {
	t, err := addMagnet(m.client, magnet)
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
	paths := key.paths(m.settings.HlsPath())
	if mkdirErr := os.MkdirAll(paths.outDir, 0755); mkdirErr != nil {
		return "", "", nil, fmt.Errorf("mkdir %s: %w", paths.outDir, mkdirErr)
	}

	m.mu.Lock()
	s, exists := m.active[key]
	if exists {
		s.mtx.Lock()
		s.lastView = time.Now()
		needResume := s.paused || s.cancel == nil
		s.mtx.Unlock()
		if needResume {
			m.startConversionLocked(key, s)
		}
		m.mu.Unlock()
		if err := s.waitReady(ctx); err != nil {
			return "", "", nil, err
		}
		return s.paths.masterPlaylist, s.paths.outDir, s.cancel, nil
	}
	m.mu.Unlock()

	source, err := newTorrentSource(file, m.sources)
	if err != nil {
		return "", "", nil, err
	}

	m.mu.Lock()
	s, exists = m.active[key]
	if exists {
		source.Close()
		s.mtx.Lock()
		s.lastView = time.Now()
		needResume := s.paused || s.cancel == nil
		s.mtx.Unlock()
		if needResume {
			m.startConversionLocked(key, s)
		}
		m.mu.Unlock()
		if err := s.waitReady(ctx); err != nil {
			return "", "", nil, err
		}
		return s.paths.masterPlaylist, s.paths.outDir, s.cancel, nil
	}

	ready := make(chan struct{})
	streamCtx, cancel := context.WithCancel(context.Background())
	s = &streamInfo{
		ready:     ready,
		cancel:    cancel,
		torrent:   t,
		file:      file,
		lastView:  time.Now(),
		paths:     paths,
		source:    source,
		selection: ffmpeg.StreamSelection{AudioTrackIndex: audioTrack, SubtitleTrackIndex: subtitleTrack},
		running:   true,
	}
	m.active[key] = s
	m.torrents[hash]++
	m.mu.Unlock()

	file.Download()
	file.SetPriority(torrent.PiecePriorityHigh)
	source.PrefetchRange(0, initialProbeBytes)

	m.launchConversion(streamCtx, ctx, key, s)
	if err := s.waitReady(ctx); err != nil {
		m.cleanupIfCurrent(key, s)
		return "", "", nil, err
	}
	log.Printf("Stream ready: key=%v, playlist=%s", key, paths.masterPlaylist)
	return paths.masterPlaylist, paths.outDir, cancel, nil
}

func (m *manager) launchConversion(
	streamCtx context.Context,
	startupCtx context.Context,
	key streamKey,
	s *streamInfo,
) {
	go func() {
		err := m.runConversion(streamCtx, startupCtx, s)
		m.finishConversion(key, s, err)
	}()
}

func (m *manager) runConversion(
	streamCtx context.Context,
	startupCtx context.Context,
	s *streamInfo,
) error {
	if startupCtx == nil {
		startupCtx = context.Background()
	}
	if err := s.source.WaitRange(startupCtx, 0, initialProbeBytes); err != nil {
		s.signalReady(err)
		return err
	}

	ffmpegHandler := ffmpeg.NewURLConverter(
		streamCtx,
		s.source.URL(),
		s.paths.outDir,
		s.paths.videoPlaylist,
		s.paths.subtitlePlaylist,
		s.paths.masterPlaylist,
		s.selection,
	)
	errCh := make(chan error, 1)
	go func() {
		errCh <- ffmpegHandler.ConvertToHLS()
	}()

	playlistReady := make(chan error, 1)
	go func() { playlistReady <- waitForPlaylist(startupCtx, s.paths.masterPlaylist) }()

	startupDone := startupCtx.Done()
	playlistReadyCh := playlistReady
	readySent := false

	for {
		select {
		case err := <-errCh:
			if !readySent {
				s.signalReady(err)
			}
			return err
		case <-startupDone:
			err := startupCtx.Err()
			if !readySent {
				s.signalReady(err)
			}
			return err
		case err := <-playlistReadyCh:
			if err != nil {
				if !readySent {
					s.signalReady(err)
				}
				return err
			}
			s.signalReady(nil)
			readySent = true
			startupDone = nil
			playlistReadyCh = nil
		}
	}
}

func (m *manager) finishConversion(key streamKey, s *streamInfo, err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("Stream conversion error for key=%v: %v", key, err)
	}

	m.mu.Lock()
	if current, ok := m.active[key]; !ok || current != s {
		m.mu.Unlock()
		return
	}
	s.running = false
	if errors.Is(err, context.Canceled) {
		s.paused = true
	}
	m.mu.Unlock()

	if err != nil && !errors.Is(err, context.Canceled) {
		m.cleanupIfCurrent(key, s)
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
