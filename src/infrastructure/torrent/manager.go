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
	"github.com/anacrolix/torrent/metainfo"
)

type manager struct {
	client      *torrent.Client
	active      map[streamKey]*streamInfo
	torrents    map[string]int
	sources     *rangeServer
	downloads   *downloadStore
	events      *downloadEventBroadcaster
	mu          sync.Mutex
	cacheMu     sync.Mutex
	hlsReserved int64
	transcodes  chan struct{}
	settings    settings.Settings
}

func NewManager(settings settings.Settings) (application.TorrentManager, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = settings.DownloadPath()
	cfg.ListenPort = settings.TorrentPort()
	// A capped cache can evict a piece after the torrent was briefly complete.
	// Keep seed connections so a later seek can fetch that piece again.
	cfg.DropMutuallyCompletePeers = false
	pieceCache, err := newPieceCache(settings.DownloadPath(), settings.MaxTorrentCacheBytes())
	if err != nil {
		return nil, err
	}
	cfg.DefaultStorage = pieceCache
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
	if err := downloads.discardLegacyPayloads(); err != nil {
		return nil, fmt.Errorf("discard legacy torrent cache: %w", err)
	}
	m := &manager{
		client:     client,
		active:     make(map[streamKey]*streamInfo),
		torrents:   make(map[string]int),
		sources:    sources,
		downloads:  downloads,
		events:     newDownloadEventBroadcaster(),
		transcodes: make(chan struct{}, settings.MaxTranscodes()),
		settings:   settings,
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
	if _, err := m.downloads.upsert(ctx, t.InfoHash().HexString(), magnet, result); err != nil {
		log.Printf("GetTorrentFiles: failed to write download metadata: %v", err)
	} else {
		m.notifyDownloadsChanged()
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
	if err := validatePieceCacheCapacity(t.Info().PieceLength, m.settings.MaxTorrentCacheBytes()); err != nil {
		return domain.MediaInfo{}, err
	}
	m.touchDownload(ctx, t.InfoHash().HexString())
	file := files[fileIndex]
	source, err := newTorrentSource(file, m.sources, m.settings.TorrentReadaheadBytes(), nil, nil)
	if err != nil {
		return domain.MediaInfo{}, err
	}
	defer source.Close()
	info, err := source.Probe(ctx)
	if err == nil && info.VideoCodec == "" {
		return domain.MediaInfo{}, errors.New("selected file has no video stream")
	}
	return info, err
}

func (m *manager) PrepareHlsStream(ctx context.Context, magnet string, fileIndex, audioTrack, subtitleTrack int, startSeconds float64) (string, error) {
	t, err := addMagnet(m.client, magnet)
	if err != nil {
		log.Printf("PrepareHlsStream: AddMagnet failed: %v", err)
		return "", err
	}
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
	if err := validatePieceCacheCapacity(t.Info().PieceLength, m.settings.MaxTorrentCacheBytes()); err != nil {
		return "", err
	}
	file := files[fileIndex]
	hash := t.InfoHash().HexString()
	m.touchDownload(ctx, hash)
	key := streamKey{InfoHash: hash, Index: fileIndex, Audio: audioTrack, Subtitle: subtitleTrack}
	paths := key.paths(m.settings.HlsPath())

	m.mu.Lock()
	s, exists := m.active[key]
	failed := false
	if exists {
		s.mtx.Lock()
		s.lastView = time.Now()
		failed = s.fatalErr != nil || s.closing
		s.mtx.Unlock()
	}
	m.mu.Unlock()
	if failed {
		m.cleanupIfCurrent(key, s)
		exists = false
	}

	if !exists {
		var candidate *streamInfo
		source, sourceErr := newTorrentSource(file, m.sources, m.settings.TorrentReadaheadBytes(), func(n int64) {
			if candidate != nil {
				candidate.recordSourceBytes(n)
			}
		}, nil)
		if sourceErr != nil {
			return "", sourceErr
		}

		streamCtx, cancel := context.WithCancel(context.Background())
		now := time.Now()
		candidate = &streamInfo{
			cancel:    cancel,
			ctx:       streamCtx,
			torrent:   t,
			file:      file,
			lastView:  time.Now(),
			paths:     paths,
			source:    source,
			selection: ffmpeg.StreamSelection{AudioTrackIndex: audioTrack, SubtitleTrackIndex: subtitleTrack},
			ready:     make(chan struct{}),
			status: domain.HlsStatus{
				Phase:         "probing",
				TargetSeconds: startSeconds,
				StartedAt:     now,
				LastProgress:  now,
			},
			statusSegment: -1,
			segmentErrors: make(map[int]segmentFailure),
			lastTorrentBytes: func() int64 {
				stats := t.Stats()
				return stats.BytesReadUsefulData.Int64()
			}(),
			videoJobs:    make(map[*segmentJob]struct{}),
			subtitleJobs: make(map[*segmentJob]struct{}),
		}

		m.mu.Lock()
		if current, ok := m.active[key]; ok {
			s = current
			m.mu.Unlock()
			cancel()
			source.Close()
		} else {
			s = candidate
			m.active[key] = s
			m.torrents[hash]++
			m.mu.Unlock()
			go m.initializeOnDemandStream(key, s)
			m.notifyDownloadsChanged()
		}
	}
	log.Printf("Stream registered: key=%v, playlist=%s", key, paths.masterPlaylist)
	return paths.masterPlaylist, nil
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

	keys := m.streamKeysForDownload(id)
	for _, key := range keys {
		m.cleanup(key)
	}
	if t, ok := m.client.Torrent(metainfo.NewHashFromHex(id)); ok {
		t.Drop()
	}

	m.mu.Lock()
	delete(m.torrents, id)
	m.mu.Unlock()

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
	for key := range m.active {
		status := domain.DownloadStatusStreaming
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
