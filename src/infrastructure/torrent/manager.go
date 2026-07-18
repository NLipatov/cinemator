package torrent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cinemator/application"
	"cinemator/domain"
	"cinemator/infrastructure/cli"
	"cinemator/infrastructure/ffmpeg"
	"cinemator/presentation/settings"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

type manager struct {
	client      *torrent.Client
	active      map[streamKey]*streamInfo
	mediaInfo   map[mediaKey]domain.MediaInfo
	torrentUses map[string]*torrentUse
	sources     *rangeServer
	downloads   *downloadStore
	assets      *hlsAssetStore
	pieces      *pieceCacheProvider
	events      *downloadEventBroadcaster
	mu          sync.Mutex
	torrentMu   sync.Mutex
	cacheMu     sync.Mutex
	hlsReserved int64
	hlsDisk     *diskBudget
	ownership   *cacheOwnership
	cleanupWG   sync.WaitGroup
	watcherStop chan struct{}
	watcherDone chan struct{}
	closeOnce   sync.Once
	closeErr    error
	transcodes  chan struct{}
	jobs        chan struct{}
	settings    settings.Settings
}

type torrentUse struct {
	torrent      *torrent.Torrent
	refs         int
	dropWhenIdle bool
	lastUsed     time.Time
}

func NewManager(settings settings.Settings) (application.TorrentManager, error) {
	if err := os.MkdirAll(settings.HlsPath(), 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(settings.HlsPath(), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(settings.DownloadPath(), 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(settings.DownloadPath(), 0700); err != nil {
		return nil, err
	}
	if err := validateCacheRoots(settings.HlsPath(), settings.DownloadPath()); err != nil {
		return nil, err
	}
	ownership, err := acquireCacheOwnership(settings.HlsPath(), settings.DownloadPath())
	if err != nil {
		return nil, err
	}
	keepOwnership := false
	defer func() {
		if !keepOwnership {
			_ = ownership.Close()
		}
	}()
	diskBudgets, err := newDiskBudgets(
		[]string{settings.HlsPath(), settings.DownloadPath()},
		settings.MinFreeBytes(),
		settings.MinFreeInodes(),
	)
	if err != nil {
		return nil, fmt.Errorf("configure disk admission: %w", err)
	}
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = settings.DownloadPath()
	cfg.ListenPort = settings.TorrentPort()
	// The bounded store routinely makes readers retry evicted pieces, and a
	// completed direct window cancels its source read. The dependency logs both
	// as errors even though Cinemator handles them, so keep only critical events.
	cfg.Slogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	// A capped cache can evict a piece after the torrent was briefly complete.
	// Keep seed connections so a later seek can fetch that piece again.
	cfg.DropMutuallyCompletePeers = false
	pieceCache, err := newPieceCache(settings.DownloadPath(), settings.MaxTorrentCacheBytes(), diskBudgets[settings.DownloadPath()])
	if err != nil {
		return nil, err
	}
	cfg.DefaultStorage = pieceCache
	assets, err := newHlsAssetStore(settings.HlsPath())
	if err != nil {
		return nil, fmt.Errorf("open HLS asset store: %w", err)
	}
	if err := discardPreviousHlsStreams(assets, settings.HlsPath()); err != nil {
		return nil, fmt.Errorf("discard previous HLS streams: %w", err)
	}
	downloads, err := newDownloadStore(settings.DownloadPath())
	if err != nil {
		return nil, err
	}
	if err := downloads.discardLegacyPayloads(); err != nil {
		return nil, fmt.Errorf("discard legacy torrent cache: %w", err)
	}
	client, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	keepClient := false
	defer func() {
		if !keepClient {
			client.Close()
		}
	}()
	sources, err := newRangeServer()
	if err != nil {
		return nil, err
	}
	m := &manager{
		client:      client,
		active:      make(map[streamKey]*streamInfo),
		mediaInfo:   make(map[mediaKey]domain.MediaInfo),
		torrentUses: make(map[string]*torrentUse),
		sources:     sources,
		downloads:   downloads,
		assets:      assets,
		pieces:      pieceCache.provider,
		hlsDisk:     diskBudgets[settings.HlsPath()],
		ownership:   ownership,
		watcherStop: make(chan struct{}),
		watcherDone: make(chan struct{}),
		events:      newDownloadEventBroadcaster(),
		transcodes:  make(chan struct{}, settings.MaxTranscodes()),
		jobs:        make(chan struct{}, settings.MaxQueuedJobs()),
		settings:    settings,
	}
	keepOwnership = true
	keepClient = true
	go m.viewerWatcher()
	return m, nil
}

func (m *manager) Close() error {
	m.closeOnce.Do(func() {
		close(m.watcherStop)
		<-m.watcherDone

		m.mu.Lock()
		keys := make([]streamKey, 0, len(m.active))
		for key := range m.active {
			keys = append(keys, key)
		}
		m.mu.Unlock()
		for _, key := range keys {
			m.cleanup(key)
		}
		m.cleanupWG.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.closeErr = errors.Join(m.closeErr, m.sources.Close(ctx))
		m.closeErr = errors.Join(m.closeErr, errors.Join(m.client.Close()...))
		if m.assets.hasReaders() || m.pieces.hasLeases() {
			m.closeErr = errors.Join(m.closeErr, errors.New("cache ownership retained because managed file handles did not close cleanly"))
			return
		}
		m.closeErr = errors.Join(m.closeErr, m.ownership.Close())
	})
	return m.closeErr
}

func validateCacheRoots(hls, download string) error {
	hls, err := canonicalCacheRoot(hls)
	if err != nil {
		return err
	}
	download, err = canonicalCacheRoot(download)
	if err != nil {
		return err
	}
	separator := string(os.PathSeparator)
	if hls == download || strings.HasPrefix(hls, download+separator) || strings.HasPrefix(download, hls+separator) {
		return errors.New("HLS and torrent cache roots must be disjoint")
	}
	return nil
}

func canonicalCacheRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(abs)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(abs), nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func discardPreviousHlsStreams(assets *hlsAssetStore, root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := assets.RetireTree(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (m *manager) GetTorrentFiles(ctx context.Context, magnet string) ([]domain.FileInfo, error) {
	t, hash, err := m.acquireTorrent(magnet)
	if err != nil {
		return nil, err
	}
	defer m.releaseTorrentUse(hash, false)

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
	t, hash, err := m.acquireTorrent(magnet)
	if err != nil {
		return domain.MediaInfo{}, err
	}
	defer m.releaseTorrentUse(hash, false)
	key := mediaKey{InfoHash: hash, Index: fileIndex}
	m.mu.Lock()
	cached, ok := m.mediaInfo[key]
	m.mu.Unlock()
	if ok {
		if err := ctx.Err(); err != nil {
			return domain.MediaInfo{}, err
		}
		m.touchDownload(ctx, key.InfoHash)
		return cached, nil
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
	info, err := source.Probe(cli.WithProcessGuards(ctx, m.ownership.guardFiles()...))
	if err == nil && info.VideoCodec == "" {
		return domain.MediaInfo{}, errors.New("selected file has no video stream")
	}
	if err == nil {
		m.mu.Lock()
		m.mediaInfo[key] = info
		m.mu.Unlock()
	}
	return info, err
}

func (m *manager) PrepareHlsStream(ctx context.Context, magnet string, fileIndex, audioTrack, subtitleTrack int, startSeconds float64, forceTranscode bool) (string, error) {
	if startSeconds < 0 || math.IsNaN(startSeconds) || math.IsInf(startSeconds, 0) {
		return "", fmt.Errorf("%w: bad start position", domain.ErrBadHlsRequest)
	}
	t, hash, err := m.acquireTorrent(magnet)
	if err != nil {
		log.Printf("PrepareHlsStream: AddMagnet failed: %v", err)
		return "", err
	}
	keepTorrent := false
	defer func() {
		if !keepTorrent {
			m.releaseTorrentUse(hash, false)
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
		return "", fmt.Errorf("%w: bad file index", domain.ErrBadHlsRequest)
	}
	if err := validatePieceCacheCapacity(t.Info().PieceLength, m.settings.MaxTorrentCacheBytes()); err != nil {
		return "", err
	}
	file := files[fileIndex]
	m.touchDownload(ctx, hash)
	startIndex := int(math.Floor(startSeconds / m.settings.HlsSegmentDuration().Seconds()))
	startIndex, _ = segmentWindow(startIndex, 0, m.settings.HlsWindowSegments())
	startMillis := int(math.Round(startSeconds * 1000))
	key := streamKey{InfoHash: hash, Index: fileIndex, Audio: audioTrack, Subtitle: subtitleTrack, Transcode: forceTranscode, Start: startMillis}
	probeKey := mediaKey{InfoHash: hash, Index: fileIndex}
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
		m.mu.Lock()
		cachedInfo, hasCachedInfo := m.mediaInfo[probeKey]
		m.mu.Unlock()
		source, sourceErr := newTorrentSource(file, m.sources, m.settings.TorrentReadaheadBytes(), func(jobID string, n int64) {
			if candidate != nil {
				candidate.recordSourceBytes(jobID, n)
			}
		}, nil)
		if sourceErr != nil {
			return "", sourceErr
		}

		streamCtx, cancel := context.WithCancel(cli.WithProcessGuards(context.Background(), m.ownership.guardFiles()...))
		now := time.Now()
		candidate = &streamInfo{
			cancel:             cancel,
			ctx:                streamCtx,
			torrent:            t,
			file:               file,
			lastView:           time.Now(),
			paths:              paths,
			assetVersion:       fmt.Sprintf("%x", now.UnixNano()),
			source:             source,
			selection:          ffmpeg.StreamSelection{AudioTrackIndex: audioTrack, SubtitleTrackIndex: subtitleTrack, ForceTranscode: forceTranscode},
			mediaInfo:          cachedInfo,
			mediaInfoReady:     hasCachedInfo,
			presentationTarget: startSeconds,
			ready:              make(chan struct{}),
			status: domain.HlsStatus{
				Phase:         "probing",
				TargetSeconds: startSeconds,
				StartedAt:     now,
				LastProgress:  now,
			},
			statusSegment:         -1,
			progressiveAdvertised: startIndex,
			progressiveSubtitles:  startIndex,
			segmentErrors:         make(map[int]segmentFailure),
			lastTorrentBytes: func() int64 {
				stats := t.Stats()
				return stats.BytesReadUsefulData.Int64()
			}(),
			videoJobs:     make(map[*segmentJob]struct{}),
			subtitleJobs:  make(map[*segmentJob]struct{}),
			directWindows: make(map[int][]ffmpeg.HLSFragment),
			cleanupDone:   make(chan struct{}),
		}

		// Keep stream publication atomic with the cache cleaner's active-stream
		// snapshot and stale-tree reset.
		m.cacheMu.Lock()
		m.mu.Lock()
		if current, ok := m.active[key]; ok {
			s = current
			m.mu.Unlock()
			m.cacheMu.Unlock()
			cancel()
			source.Close()
		} else if len(m.active) >= m.settings.MaxActiveStreams() {
			m.mu.Unlock()
			m.cacheMu.Unlock()
			cancel()
			source.Close()
			return "", fmt.Errorf("%w: active stream limit reached (%d)", domain.ErrHlsTemporarilyUnavailable, m.settings.MaxActiveStreams())
		} else {
			// Remove playlists from an earlier process before publishing the
			// stream. Otherwise a recovering player can read a stale master
			// while initialization is still probing the torrent.
			if resetErr := m.resetStreamOutput(paths); resetErr != nil {
				m.mu.Unlock()
				m.cacheMu.Unlock()
				cancel()
				source.Close()
				return "", resetErr
			}
			s = candidate
			m.active[key] = s
			keepTorrent = true
			m.mu.Unlock()
			m.cacheMu.Unlock()
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
	m.torrentMu.Lock()
	if use := m.torrentUses[id]; use != nil && use.refs > 0 {
		m.torrentMu.Unlock()
		return fmt.Errorf("download is in use")
	}
	delete(m.torrentUses, id)
	if t, ok := m.client.Torrent(metainfo.NewHashFromHex(id)); ok {
		t.Drop()
	}
	m.torrentMu.Unlock()

	m.mu.Lock()
	for key := range m.mediaInfo {
		if key.InfoHash == id {
			delete(m.mediaInfo, key)
		}
	}
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
	changed, err := m.downloads.touch(ctx, id)
	if err != nil && !errors.Is(err, domain.ErrDownloadNotFound) {
		log.Printf("failed to touch download metadata: %v", err)
	} else if changed {
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
	m.torrentMu.Lock()
	inUse := m.torrentUses[id] != nil && m.torrentUses[id].refs > 0
	m.torrentMu.Unlock()
	if inUse {
		return true
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.active {
		if key.InfoHash == id {
			return true
		}
	}
	return false
}

func (m *manager) acquireTorrent(magnet string) (*torrent.Torrent, string, error) {
	m.torrentMu.Lock()
	defer m.torrentMu.Unlock()
	t, err := addMagnet(m.client, magnet)
	if err != nil {
		return nil, "", err
	}
	hash := t.InfoHash().HexString()
	use := m.torrentUses[hash]
	if use == nil {
		use = &torrentUse{}
		m.torrentUses[hash] = use
	}
	use.torrent = t
	use.refs++
	use.lastUsed = time.Now()
	return t, hash, nil
}

func (m *manager) releaseTorrentUse(hash string, dropWhenIdle bool) {
	m.torrentMu.Lock()
	m.releaseTorrentUseLocked(hash, dropWhenIdle)
	m.torrentMu.Unlock()
}

func (m *manager) releaseTorrentUseLocked(hash string, dropWhenIdle bool) {
	use := m.torrentUses[hash]
	if use == nil || use.refs <= 0 {
		return
	}
	use.dropWhenIdle = use.dropWhenIdle || dropWhenIdle
	use.refs--
	use.lastUsed = time.Now()
	if use.refs != 0 {
		return
	}
	if use.dropWhenIdle {
		delete(m.torrentUses, hash)
		if use.torrent != nil {
			use.torrent.Drop()
		}
		return
	}
	m.trimIdleTorrentUsesLocked()
}

func (m *manager) trimIdleTorrentUsesLocked() {
	limit := m.settings.MaxActiveStreams()
	idle := 0
	oldestHash := ""
	var oldest *torrentUse
	for hash, use := range m.torrentUses {
		if use.refs != 0 || use.dropWhenIdle {
			continue
		}
		idle++
		if oldest == nil || use.lastUsed.Before(oldest.lastUsed) {
			oldestHash = hash
			oldest = use
		}
	}
	if idle <= limit || oldest == nil {
		return
	}
	delete(m.torrentUses, oldestHash)
	if oldest.torrent != nil {
		oldest.torrent.Drop()
	}
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
		if err := m.assets.RetireTree(filepath.Join(m.settings.HlsPath(), entry.Name())); err != nil && !errors.Is(err, errHlsAssetsBusy) {
			return err
		}
	}
	return nil
}
