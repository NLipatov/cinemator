package torrent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cinemator/application"
	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
	"cinemator/presentation/settings"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
)

type manager struct {
	client     *torrent.Client
	active     map[streamKey]*streamInfo
	streamOps  map[streamKey]chan struct{} // Serializes output lifecycle changes for each stream.
	torrents   map[string]int              // References held by preparing, active, and cleaning streams.
	torrentOps map[string]chan struct{}    // Serializes add and drop for each torrent.
	deletions  map[string]chan struct{}    // Prevents new streams while a download is being deleted.
	sources    *rangeServer
	downloads  *downloadStore
	events     *downloadEventBroadcaster
	mu         sync.Mutex
	settings   settings.Settings
}

func NewManager(settings settings.Settings) (application.TorrentManager, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = settings.DownloadPath()
	cfg.ListenPort = settings.TorrentPort()
	cfg.DefaultStorage = storage.NewFileByInfoHash(settings.DownloadPath())
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
	downloads, err := newDownloadStore(settings.DownloadPath())
	if err != nil {
		return nil, err
	}
	m := &manager{
		client:     client,
		active:     make(map[streamKey]*streamInfo),
		streamOps:  make(map[streamKey]chan struct{}),
		torrents:   make(map[string]int),
		torrentOps: make(map[string]chan struct{}),
		deletions:  make(map[string]chan struct{}),
		sources:    sources,
		downloads:  downloads,
		events:     newDownloadEventBroadcaster(),
		settings:   settings,
	}
	go m.viewerWatcher()
	return m, nil
}

func (m *manager) GetTorrentFiles(ctx context.Context, magnet string) ([]domain.FileInfo, error) {
	t, hash, err := m.retainTorrent(ctx, magnet)
	if err != nil {
		return nil, err
	}
	defer m.releaseTorrent(hash, t)

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
	if _, err := m.downloads.upsert(ctx, hash, magnet, result); err != nil {
		log.Printf("GetTorrentFiles: failed to write download metadata: %v", err)
	} else {
		m.notifyDownloadsChanged()
	}
	return result, nil
}

func (m *manager) GetMediaInfo(ctx context.Context, magnet string, fileIndex int) (domain.MediaInfo, error) {
	t, hash, err := m.retainTorrent(ctx, magnet)
	if err != nil {
		return domain.MediaInfo{}, err
	}
	defer m.releaseTorrent(hash, t)
	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return domain.MediaInfo{}, ctx.Err()
	}
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return domain.MediaInfo{}, fmt.Errorf("bad file index")
	}
	m.touchDownload(ctx, hash)
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

func (m *manager) PrepareHlsStream(ctx context.Context, magnet string, fileIndex, audioTrack, subtitleTrack int) (string, error) {
	t, hash, err := m.retainTorrent(ctx, magnet)
	if err != nil {
		log.Printf("PrepareHlsStream: AddMagnet failed: %v", err)
		return "", err
	}
	torrentRetained := true
	defer func() {
		if torrentRetained {
			m.releaseTorrent(hash, t)
		}
	}()

	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return "", ctx.Err()
	}
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		log.Printf("PrepareHlsStream: bad file index: %d", fileIndex)
		return "", fmt.Errorf("bad file index")
	}
	file := files[fileIndex]
	m.touchDownload(ctx, hash)
	key := streamKey{InfoHash: hash, Index: fileIndex, Audio: audioTrack, Subtitle: subtitleTrack}
	paths := key.paths(m.settings.HlsPath())

	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if s, runID, resumed, exists, err := m.getOrResumeStream(ctx, key); exists {
			if err != nil {
				return "", err
			}
			if resumed {
				m.notifyDownloadsChanged()
			}
			return m.waitForPlayableStream(ctx, key, s, runID)
		}

		source, err := newTorrentSource(file, m.sources)
		if err != nil {
			return "", err
		}

		m.mu.Lock()
		if operationDone := m.streamOps[key]; operationDone != nil {
			m.mu.Unlock()
			source.Close()
			if err := waitForDone(ctx, operationDone); err != nil {
				return "", err
			}
			continue
		}
		if _, exists := m.active[key]; exists {
			m.mu.Unlock()
			source.Close()
			continue
		}
		operationDone := m.reserveStreamOperationLocked(key)
		m.mu.Unlock()

		if err := resetStreamOutput(paths); err != nil {
			source.Close()
			m.finishStreamOperation(key, operationDone)
			return "", fmt.Errorf("reset stream output %s: %w", paths.outDir, err)
		}

		streamCtx, cancel := context.WithCancel(context.Background())
		s := &streamInfo{
			cancel:    cancel,
			torrent:   t,
			file:      file,
			lastView:  time.Now(),
			paths:     paths,
			source:    source,
			selection: ffmpeg.StreamSelection{AudioTrackIndex: audioTrack, SubtitleTrackIndex: subtitleTrack},
			running:   true,
		}
		runID := s.beginRun()
		s.registerStartupWaiter()
		m.mu.Lock()
		m.active[key] = s
		torrentRetained = false
		m.mu.Unlock()

		file.Download()
		file.SetPriority(torrent.PiecePriorityHigh)
		source.PrefetchRange(0, initialProbeBytes)

		m.launchConversion(streamCtx, key, s, runID)
		m.finishStreamOperation(key, operationDone)
		m.notifyDownloadsChanged()
		playlist, err := m.waitForPlayableStream(ctx, key, s, runID)
		if err != nil {
			return "", err
		}
		log.Printf("Stream ready: key=%v, playlist=%s", key, paths.masterPlaylist)
		return playlist, nil
	}
}

func (m *manager) getOrResumeStream(ctx context.Context, key streamKey) (*streamInfo, uint64, bool, bool, error) {
	for {
		m.mu.Lock()
		if operationDone := m.streamOps[key]; operationDone != nil {
			m.mu.Unlock()
			if err := waitForDone(ctx, operationDone); err != nil {
				return nil, 0, false, true, err
			}
			continue
		}

		s, exists := m.active[key]
		if !exists {
			m.mu.Unlock()
			return nil, 0, false, false, nil
		}
		s.mtx.Lock()
		s.lastView = time.Now()
		needResume := !s.completed && (s.paused || s.cancel == nil)
		oldRunDone := s.runDone
		s.mtx.Unlock()
		if !needResume {
			runID := s.registerStartupWaiter()
			m.mu.Unlock()
			return s, runID, false, true, nil
		}

		operationDone := m.reserveStreamOperationLocked(key)
		m.mu.Unlock()
		if oldRunDone != nil {
			if err := waitForDone(ctx, oldRunDone); err != nil {
				m.finishStreamOperation(key, operationDone)
				return s, 0, false, true, err
			}
		}
		if err := resetStreamOutput(s.paths); err != nil {
			m.finishStreamOperation(key, operationDone)
			return s, 0, false, true, fmt.Errorf("reset stream output %s: %w", s.paths.outDir, err)
		}

		streamCtx, cancel := context.WithCancel(context.Background())
		m.mu.Lock()
		if current, active := m.active[key]; !active || current != s {
			m.mu.Unlock()
			cancel()
			m.finishStreamOperation(key, operationDone)
			continue
		}
		s.cancel = cancel
		s.paused = false
		s.running = true
		runID := s.beginRun()
		s.registerStartupWaiter()
		m.mu.Unlock()

		s.file.SetPriority(torrent.PiecePriorityHigh)
		m.launchConversion(streamCtx, key, s, runID)
		m.finishStreamOperation(key, operationDone)
		return s, runID, true, true, nil
	}
}

func (m *manager) waitForPlayableStream(ctx context.Context, key streamKey, s *streamInfo, runID uint64) (string, error) {
	err := s.waitPlayable(ctx)
	requestCanceled := ctx != nil && ctx.Err() != nil
	m.releaseStartupWaiter(key, s, runID, requestCanceled)
	if err != nil {
		return "", err
	}
	return s.paths.masterPlaylist, nil
}

func (m *manager) ListDownloads(ctx context.Context) ([]domain.Download, error) {
	downloads, err := m.downloads.list(ctx)
	if err != nil {
		return nil, err
	}
	statuses := m.downloadStatuses()
	for i := range downloads {
		if downloads[i].Status == domain.DownloadStatusExpired {
			continue
		}
		if status, ok := statuses[downloads[i].ID]; ok {
			downloads[i].Status = status
		}
	}
	return downloads, nil
}

func (m *manager) ExtendDownload(ctx context.Context, id string, extension time.Duration) (domain.Download, error) {
	download, err := m.downloads.extend(ctx, id, extension)
	if err != nil {
		return domain.Download{}, err
	}
	if status, ok := m.downloadStatuses()[download.ID]; ok && download.Status != domain.DownloadStatusExpired {
		download.Status = status
	}
	m.notifyDownloadsChanged()
	return download, nil
}

func (m *manager) DeleteDownload(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return err
	}
	deletionDone, err := m.reserveDownloadDeletion(ctx, id)
	if err != nil {
		return err
	}
	defer m.finishDownloadDeletion(id, deletionDone)

	keys := m.streamKeysForDownload(id)
	for _, key := range keys {
		if err := m.cleanup(ctx, key); err != nil {
			return err
		}
	}
	if err := m.dropTorrent(ctx, id); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := m.removeDownloadHlsDirs(id); err != nil {
		return err
	}
	if err := m.downloads.delete(ctx, id); err != nil {
		return err
	}
	m.notifyDownloadsChanged()
	return nil
}

func (m *manager) touchDownload(ctx context.Context, id string) {
	if err := m.downloads.touch(ctx, id); err != nil && !errors.Is(err, domain.ErrDownloadNotFound) {
		log.Printf("failed to touch download metadata: %v", err)
	} else if err == nil {
		m.notifyDownloadsChanged()
	}
}

func (m *manager) SubscribeDownloadEvents(ctx context.Context) <-chan struct{} {
	return m.events.subscribe(ctx)
}

func (m *manager) notifyDownloadsChanged() {
	if m.events == nil {
		return
	}
	m.events.notify()
}

func (m *manager) downloadStatuses() map[string]domain.DownloadStatus {
	statuses := make(map[string]domain.DownloadStatus)
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, stream := range m.active {
		status := domain.DownloadStatusStreaming
		stream.mtx.Lock()
		if stream.completed {
			status = domain.DownloadStatusReady
		} else if stream.paused {
			status = domain.DownloadStatusPaused
		}
		stream.mtx.Unlock()

		current, ok := statuses[key.InfoHash]
		if !ok || current != domain.DownloadStatusStreaming {
			statuses[key.InfoHash] = status
		}
	}
	return statuses
}

func (m *manager) streamKeysForDownload(id string) []streamKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]streamKey, 0)
	for key := range m.active {
		if key.InfoHash == id {
			keys = append(keys, key)
		}
	}
	for key := range m.streamOps {
		if key.InfoHash == id {
			keys = append(keys, key)
		}
	}
	return keys
}

func (m *manager) downloadActive(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.active {
		if key.InfoHash == id {
			return true
		}
	}
	return false
}

func (m *manager) cleanupExpiredDownloads(ctx context.Context) {
	ids, err := m.downloads.expiredIDs(ctx, time.Now().UTC())
	if err != nil {
		log.Printf("cleanupExpiredDownloads: failed to list expired downloads: %v", err)
		return
	}
	for _, id := range ids {
		if m.downloadActive(id) {
			continue
		}
		if err := m.DeleteDownload(ctx, id); err != nil {
			log.Printf("cleanupExpiredDownloads: failed to delete %s: %v", id, err)
		}
	}
}

func (m *manager) removeDownloadHlsDirs(id string) error {
	entries, err := os.ReadDir(m.settings.HlsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	prefix := id + "_"
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.settings.HlsPath(), entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (m *manager) launchConversion(
	streamCtx context.Context,
	key streamKey,
	s *streamInfo,
	runID uint64,
) {
	runDone := s.runDone
	go func() {
		err := m.runConversion(streamCtx, s, runID)
		close(runDone)
		m.finishConversion(key, s, runID, err)
	}()
}

func (m *manager) runConversion(
	streamCtx context.Context,
	s *streamInfo,
	runID uint64,
) error {
	if err := s.source.WaitRange(streamCtx, 0, initialProbeBytes); err != nil {
		s.signalPlayable(runID, err)
		return err
	}

	conversionCtx, cancelConversion := context.WithCancel(streamCtx)
	defer cancelConversion()
	ffmpegHandler := ffmpeg.NewURLConverter(
		conversionCtx,
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
	go func() { playlistReady <- waitForPlaylist(conversionCtx, s.paths.masterPlaylist) }()

	streamDone := streamCtx.Done()
	playlistReadyCh := playlistReady
	playableSent := false

	for {
		select {
		case err := <-errCh:
			if !playableSent {
				s.signalPlayable(runID, err)
			}
			return err
		case <-streamDone:
			err := streamCtx.Err()
			if !playableSent {
				s.signalPlayable(runID, err)
			}
			cancelConversion()
			<-errCh
			return err
		case err := <-playlistReadyCh:
			if err != nil {
				if !playableSent {
					s.signalPlayable(runID, err)
				}
				cancelConversion()
				<-errCh
				return err
			}
			s.signalPlayable(runID, nil)
			playableSent = true
			streamDone = nil
			playlistReadyCh = nil
		}
	}
}

func (m *manager) finishConversion(key streamKey, s *streamInfo, runID uint64, err error) {
	m.mu.Lock()
	if current, ok := m.active[key]; !ok || current != s || !s.isCurrentRun(runID) {
		m.mu.Unlock()
		return
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("Stream conversion error for key=%v: %v", key, err)
	}
	s.running = false
	notifyCompleted := false
	if err == nil {
		s.completed = true
		s.paused = false
		notifyCompleted = true
	} else if errors.Is(err, context.Canceled) {
		s.paused = true
	}
	m.mu.Unlock()
	if notifyCompleted {
		m.notifyDownloadsChanged()
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		m.cleanupIfCurrentRun(key, s, runID)
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
