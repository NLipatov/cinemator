package torrent

import (
	"context"
	"encoding/hex"
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
	client       *torrent.Client
	active       map[streamKey]*streamInfo
	mediaInfo    map[mediaKey]domain.MediaInfo
	torrentUses  map[string]*torrentUse
	sources      *rangeServer
	downloads    *downloadStore
	media        *mediaCache
	events       *downloadEventBroadcaster
	mu           sync.Mutex
	torrentMu    sync.Mutex
	pieceIndexMu sync.Mutex
	pieceRefs    map[string][]cachedPieceRef
	pieceHashes  map[*torrent.Torrent][]string
	ownership    *cacheOwnership
	cleanupWG    sync.WaitGroup
	watcherStop  chan struct{}
	watcherDone  chan struct{}
	closeOnce    sync.Once
	closeErr     error
	scheduler    *segmentScheduler
	demand       *pieceDemand
	settings     settings.Settings
}

type torrentUse struct {
	torrent      *torrent.Torrent
	refs         int
	dropWhenIdle bool
	lastUsed     time.Time
}

type cachedPieceRef struct {
	torrent *torrent.Torrent
	index   int
}

const streamShutdownTimeout = 10 * time.Second

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
	cacheBudget := newCacheBudget(settings.MaxCacheBytes())
	pieceCache, err := newPieceCache(settings.DownloadPath(), cacheBudget, diskBudgets[settings.DownloadPath()])
	if err != nil {
		return nil, err
	}
	cfg.DefaultStorage = pieceCache
	assets, err := newHlsAssetStore(settings.HlsPath())
	if err != nil {
		return nil, fmt.Errorf("open HLS asset store: %w", err)
	}
	keepAssets := false
	defer func() {
		if !keepAssets {
			_ = assets.Close()
		}
	}()
	media := &mediaCache{
		assets:  assets,
		pieces:  pieceCache.provider,
		budget:  cacheBudget,
		hlsDisk: diskBudgets[settings.HlsPath()],
	}
	if err := media.discardHlsStreams(settings.HlsPath()); err != nil {
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
		media:       media,
		pieceRefs:   make(map[string][]cachedPieceRef),
		pieceHashes: make(map[*torrent.Torrent][]string),
		ownership:   ownership,
		watcherStop: make(chan struct{}),
		watcherDone: make(chan struct{}),
		events:      newDownloadEventBroadcaster(),
		scheduler:   newSegmentScheduler(settings.MaxQueuedJobs(), settings.MaxTranscodes()),
		demand:      newPieceDemand(),
		settings:    settings,
	}
	pieceCache.provider.onEvict = m.syncEvictedPieces
	keepOwnership = true
	keepAssets = true
	keepClient = true
	go m.viewerWatcher()
	return m, nil
}

func (m *manager) Close() error {
	m.closeOnce.Do(func() {
		close(m.watcherStop)
		<-m.watcherDone

		ctx, cancel := context.WithTimeout(context.Background(), streamShutdownTimeout)
		shutdownErr := m.shutdownStreams(ctx)
		cancel()
		if shutdownErr != nil {
			m.closeErr = errors.Join(m.closeErr, shutdownErr, errors.New("cache ownership retained because stream workers did not stop cleanly"))
			return
		}

		sourceCtx, cancelSources := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelSources()
		m.closeErr = errors.Join(m.closeErr, m.sources.Close(sourceCtx))
		m.closeErr = errors.Join(m.closeErr, errors.Join(m.client.Close()...))
		if m.media.hasOpenHandles() {
			m.closeErr = errors.Join(m.closeErr, errors.New("cache ownership retained because managed file handles did not close cleanly"))
			return
		}
		m.closeErr = errors.Join(m.closeErr, m.media.close())
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
	m.indexTorrentPieces(t)

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
	cached, ok := m.cachedMediaDescriptor(ctx, key)
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
	m.indexTorrentPieces(t)
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return domain.MediaInfo{}, fmt.Errorf("bad file index")
	}
	if err := validatePieceCacheCapacity(t.Info().PieceLength, m.settings.MaxCacheBytes()); err != nil {
		return domain.MediaInfo{}, err
	}
	m.touchDownload(ctx, t.InfoHash().HexString())
	file := files[fileIndex]
	source, err := newTorrentSource(file, m.sources, m.demand, m.settings.TorrentReadaheadBytes(), nil, nil, nil, nil)
	if err != nil {
		return domain.MediaInfo{}, err
	}
	defer source.Close()
	info, err := source.Probe(cli.WithProcessGuards(ctx, m.ownership.guardFiles()...))
	if err == nil && info.VideoCodec == "" {
		return domain.MediaInfo{}, errors.New("selected file has no video stream")
	}
	if err == nil {
		m.storeMediaDescriptor(ctx, key, info)
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
	m.indexTorrentPieces(t)
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		log.Printf("PrepareHlsStream: bad file index: %d", fileIndex)
		return "", fmt.Errorf("%w: bad file index", domain.ErrBadHlsRequest)
	}
	if err := validatePieceCacheCapacity(t.Info().PieceLength, m.settings.MaxCacheBytes()); err != nil {
		return "", err
	}
	file := files[fileIndex]
	m.touchDownload(ctx, hash)
	probeKey := mediaKey{InfoHash: hash, Index: fileIndex}
	cachedInfo, hasCachedInfo := m.cachedMediaDescriptor(ctx, probeKey)
	duration := 0.0
	if hasCachedInfo {
		duration = cachedInfo.Duration
	}
	target := m.timeline(duration).locate(startSeconds)
	startSeconds = target.sourceSeconds
	startIndex := target.segment
	key := streamKey{InfoHash: hash, Index: fileIndex, Audio: audioTrack, Subtitle: subtitleTrack, Transcode: forceTranscode}
	paths := key.paths(m.settings.HlsPath())

	for {
		m.mu.Lock()
		s, cleanupDone := m.reuseOrRetireStreamLocked(key, time.Now())
		m.mu.Unlock()
		if s != nil {
			m.requestVideoTarget(s, target)
			return paths.masterPlaylist, nil
		}
		if cleanupDone != nil {
			if err := waitForStreamCleanup(ctx, cleanupDone); err != nil {
				return "", err
			}
			continue
		}

		var candidate *streamInfo
		source, sourceErr := newTorrentSource(file, m.sources, m.demand, m.settings.TorrentReadaheadBytes(), func(jobID string) int64 {
			if candidate == nil {
				return 16 << 20
			}
			return candidate.sourceReadahead(jobID, m.settings.TorrentReadaheadBytes())
		}, func(jobID string, offset, length int64) {
			if candidate != nil {
				candidate.recordSourceRange(jobID, offset, length)
			}
		}, func(jobID string, offset, n int64) {
			if candidate != nil {
				candidate.recordSourceBytes(jobID, offset, n)
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
			mediaKey:           probeKey,
			presentationTarget: startSeconds,
			ready:              make(chan struct{}),
			status: domain.HlsStatus{
				Phase:         domain.HlsPhaseProbing,
				Stage:         domain.HlsStageWaitingSource,
				TargetSeconds: startSeconds,
				StartedAt:     now,
				LastProgress:  now,
			},
			statusSegment:         -1,
			progressiveAdvertised: startIndex,
			progressiveDemand:     startIndex,
			progressiveTarget:     30 * time.Second,
			progressiveSubtitles:  startIndex,
			playbackWindow:        startIndex,
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
		created := false
		var publishErr error
		m.media.synchronizeLifecycle(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			s, cleanupDone = m.reuseOrRetireStreamLocked(key, time.Now())
			if s != nil || cleanupDone != nil {
				return
			}
			if len(m.active) >= m.settings.MaxActiveStreams() {
				publishErr = fmt.Errorf("%w: active stream limit reached (%d)", domain.ErrHlsTemporarilyUnavailable, m.settings.MaxActiveStreams())
				return
			}
			// Remove playlists from an earlier process before publishing the
			// stream. Otherwise a recovering player can read a stale master
			// while initialization is still probing the torrent.
			if publishErr = m.resetStreamOutput(paths); publishErr != nil {
				return
			}
			s = candidate
			m.active[key] = s
			created = true
			keepTorrent = true
		})
		if !created {
			cancel()
			source.Close()
		}
		if publishErr != nil {
			return "", publishErr
		}
		if cleanupDone != nil {
			if err := waitForStreamCleanup(ctx, cleanupDone); err != nil {
				return "", err
			}
			continue
		}
		if !created {
			m.requestVideoTarget(s, target)
			return paths.masterPlaylist, nil
		}
		go m.initializeOnDemandStream(key, s)
		m.notifyDownloadsChanged()
		log.Printf("Stream registered: key=%v, playlist=%s", key, paths.masterPlaylist)
		return paths.masterPlaylist, nil
	}
}

func (m *manager) cachedMediaDescriptor(ctx context.Context, key mediaKey) (domain.MediaInfo, bool) {
	m.mu.Lock()
	info, ok := m.mediaInfo[key]
	m.mu.Unlock()
	if ok {
		return info, true
	}
	info, ok, err := m.downloads.readMediaInfo(ctx, key.InfoHash, key.Index)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("failed to read cached media descriptor: hash=%s file=%d: %v", key.InfoHash, key.Index, err)
		}
		return domain.MediaInfo{}, false
	}
	if ok {
		m.mu.Lock()
		m.mediaInfo[key] = info
		m.mu.Unlock()
	}
	return info, ok
}

func (m *manager) storeMediaDescriptor(ctx context.Context, key mediaKey, info domain.MediaInfo) {
	m.mu.Lock()
	m.mediaInfo[key] = info
	m.mu.Unlock()
	if err := m.downloads.writeMediaInfo(ctx, key.InfoHash, key.Index, info); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("failed to persist media descriptor: hash=%s file=%d: %v", key.InfoHash, key.Index, err)
	}
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
		m.unindexTorrentPieces(t)
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
			m.unindexTorrentPieces(use.torrent)
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
		m.unindexTorrentPieces(oldest.torrent)
		oldest.torrent.Drop()
	}
}

func (m *manager) indexTorrentPieces(t *torrent.Torrent) {
	m.pieceIndexMu.Lock()
	defer m.pieceIndexMu.Unlock()
	if _, indexed := m.pieceHashes[t]; indexed {
		return
	}

	info := t.Info()
	refs := make(map[string][]int)
	if info.HasV1() {
		for index := 0; index < t.NumPieces(); index++ {
			if hash := t.Piece(index).Info().V1Hash(); hash.Ok {
				key := hex.EncodeToString(hash.Value[:])
				refs[key] = append(refs[key], index)
			}
		}
	} else if info.HasV2() {
		layers := t.Metainfo().PieceLayers
		for _, file := range info.UpvertedFiles() {
			if file.Length == 0 || !file.PiecesRoot.Ok {
				continue
			}
			begin, end := file.BeginPieceIndex(info.PieceLength), file.EndPieceIndex(info.PieceLength)
			root := file.PiecesRoot.Value
			if end-begin == 1 {
				key := hex.EncodeToString(root[:])
				refs[key] = append(refs[key], begin)
				continue
			}
			layer := layers[string(root[:])]
			for index := begin; index < end; index++ {
				offset := (index - begin) * 32
				if offset+32 > len(layer) {
					break
				}
				key := hex.EncodeToString([]byte(layer[offset : offset+32]))
				refs[key] = append(refs[key], index)
			}
		}
	}

	hashes := make([]string, 0, len(refs))
	for hash, indices := range refs {
		for _, index := range indices {
			m.pieceRefs[hash] = append(m.pieceRefs[hash], cachedPieceRef{torrent: t, index: index})
		}
		hashes = append(hashes, hash)
	}
	m.pieceHashes[t] = hashes
}

func (m *manager) unindexTorrentPieces(t *torrent.Torrent) {
	m.pieceIndexMu.Lock()
	defer m.pieceIndexMu.Unlock()
	for _, hash := range m.pieceHashes[t] {
		refs := m.pieceRefs[hash]
		kept := refs[:0]
		for _, ref := range refs {
			if ref.torrent != t {
				kept = append(kept, ref)
			}
		}
		if len(kept) == 0 {
			delete(m.pieceRefs, hash)
		} else {
			m.pieceRefs[hash] = kept
		}
	}
	delete(m.pieceHashes, t)
}

func (m *manager) syncEvictedPieces(locations []string) {
	hashes := make(map[string]struct{})
	for _, location := range locations {
		parts := strings.Split(location, "/")
		if len(parts) >= 2 && parts[0] == "completed" {
			hashes[parts[1]] = struct{}{}
		}
	}
	m.pieceIndexMu.Lock()
	defer m.pieceIndexMu.Unlock()
	for hash := range hashes {
		for _, ref := range m.pieceRefs[hash] {
			ref.torrent.Piece(ref.index).UpdateCompletion()
		}
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
	return m.media.retireDownloadHls(m.settings.HlsPath(), id)
}
