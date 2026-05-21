package torrent

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	defer file.SetPriority(origPrio)
	file.Download()

	if err := waitForProbeBytes(ctx, file, int64(m.settings.MinProbeSizeMiB())<<20); err != nil {
		return domain.MediaInfo{}, err
	}

	analyzer := ffmpeg.SampleAnalyzer{}
	reader := file.NewReader()
	defer reader.Close()
	return analyzer.Analyze(reader)
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
	paths := key.paths(m.settings.HlsPath())
	if mkdirErr := os.MkdirAll(paths.outDir, 0755); mkdirErr != nil {
		return "", "", nil, fmt.Errorf("mkdir %s: %w", paths.outDir, mkdirErr)
	}

	m.mu.Lock()
	s, exists := m.active[key]
	if exists {
		s.mtx.Lock()
		s.lastView = time.Now()
		s.mtx.Unlock()
		m.mu.Unlock()
		<-s.ready
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
		selection: ffmpeg.StreamSelection{AudioTrackIndex: audioTrack, SubtitleTrackIndex: subtitleTrack},
		running:   true,
	}
	m.active[key] = s
	m.torrents[hash]++
	m.mu.Unlock()

	file.Download()
	file.SetPriority(torrent.PiecePriorityHigh)
	m.preloadLeadingPieces(file)

	if probeErr := m.convertFileToStream(streamCtx, ctx, key, s); probeErr != nil {
		m.cleanup(key)
		return "", "", nil, probeErr
	}
	log.Printf("Stream ready: key=%v, playlist=%s", key, paths.masterPlaylist)
	return paths.masterPlaylist, paths.outDir, cancel, nil
}

func (m *manager) preloadLeadingPieces(f *torrent.File) {
	const preloadPieces = 64
	begin := f.BeginPieceIndex()
	end := begin + preloadPieces
	if end > f.EndPieceIndex() {
		end = f.EndPieceIndex()
	}
	if end > begin {
		f.Torrent().DownloadPieces(begin, end)
	}
}

func (m *manager) targetReadaheadBytes(f *torrent.File) int64 {
	const (
		targetBufferBytes = int64(1 << 30)   // ~1 GiB target to cover ~10-20 minutes of content
		minAheadBytes     = int64(128 << 20) // ensure at least a short runway
	)
	ahead := targetBufferBytes
	if ahead < minAheadBytes {
		ahead = minAheadBytes
	}
	if l := f.Length(); l > 0 && l < ahead {
		ahead = l
	}
	return ahead
}
func (m *manager) convertFileToStream(
	streamCtx context.Context,
	prepareCtx context.Context,
	key streamKey,
	s *streamInfo,
) error {
	if prepareCtx == nil {
		prepareCtx = context.Background()
	}
	ready := s.ready
	if ready != nil {
		defer close(ready)
	}
	if err := waitForProbeBytes(prepareCtx, s.file, int64(m.settings.MinProbeSizeMiB())<<20); err != nil {
		return err
	}

	targetReadahead := m.targetReadaheadBytes(s.file)
	ffmpegHandler := ffmpeg.NewConverter(streamCtx, func() io.ReadCloser {
		reader := s.file.NewReader()
		reader.SetContext(streamCtx)
		reader.SetReadahead(targetReadahead) // keep a deep sequential window buffered
		reader.SetResponsive()
		return reader
	}, s.paths.outDir, s.paths.videoPlaylist, s.paths.subtitlePlaylist, s.paths.masterPlaylist, s.selection)
	errCh := make(chan error, 1)
	go func() {
		errCh <- ffmpegHandler.ConvertToHLS()
	}()

	playlistReady := make(chan error, 1)
	go func() { playlistReady <- waitForPlaylist(prepareCtx, s.paths.masterPlaylist) }()
	select {
	case <-prepareCtx.Done():
		return prepareCtx.Err()
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("FFmpeg error for key=%v: %v", key, err)
		}
		return err
	case err := <-playlistReady:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("waitForPlaylist failed for key=%v: %v", key, err)
			return err
		}
		return nil
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

func waitForProbeBytes(ctx context.Context, file *torrent.File, minBytes int64) error {
	if minBytes <= 0 {
		return nil
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if file.BytesCompleted() >= minBytes {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
