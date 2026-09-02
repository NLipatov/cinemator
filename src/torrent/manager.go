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

	"cinemator/config"
	"cinemator/media"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
)

type Manager struct {
	client         *torrent.Client
	active         map[streamKey]*streamInfo
	preparations   map[streamKey]*preparationJob
	streamOps      map[streamKey]chan struct{} // Serializes output lifecycle changes for each stream.
	torrents       map[string]int              // References held by preparing, active, and cleaning streams.
	torrentOps     map[string]chan struct{}    // Serializes add and drop for each torrent.
	preparationOps map[string]chan struct{}    // Serializes preparation selection for each torrent.
	deletions      map[string]chan struct{}    // Prevents new streams while a download is being deleted.
	sources        *rangeServer
	downloads      *downloadStore
	events         *downloadEventBroadcaster
	mu             sync.Mutex
	cfg            config.Config
}

type preparationJob struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func NewManager(appConfig config.Config) (*Manager, error) {
	torrentConfig := torrent.NewDefaultClientConfig()
	torrentConfig.DataDir = appConfig.DownloadPath
	torrentConfig.ListenPort = appConfig.TorrentPort
	torrentConfig.DefaultStorage = storage.NewFileByInfoHash(appConfig.DownloadPath)
	client, err := torrent.NewClient(torrentConfig)
	if err != nil {
		return nil, err
	}
	if mkdirErr := os.MkdirAll(appConfig.HLSPath, 0755); mkdirErr != nil {
		return nil, mkdirErr
	}
	if mkdirErr := os.MkdirAll(appConfig.DownloadPath, 0755); mkdirErr != nil {
		return nil, mkdirErr
	}
	sources, err := newRangeServer()
	if err != nil {
		return nil, err
	}
	downloads, err := newDownloadStore(appConfig.DownloadPath)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		client:         client,
		active:         make(map[streamKey]*streamInfo),
		preparations:   make(map[streamKey]*preparationJob),
		streamOps:      make(map[streamKey]chan struct{}),
		torrents:       make(map[string]int),
		torrentOps:     make(map[string]chan struct{}),
		preparationOps: make(map[string]chan struct{}),
		deletions:      make(map[string]chan struct{}),
		sources:        sources,
		downloads:      downloads,
		events:         newDownloadEventBroadcaster(),
		cfg:            appConfig,
	}
	go m.viewerWatcher()
	go m.resumePreparations()
	return m, nil
}

func (m *Manager) GetTorrentFiles(ctx context.Context, magnet string) ([]FileInfo, error) {
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
	result := make([]FileInfo, len(files))
	for i, f := range files {
		result[i] = FileInfo{Index: i, Name: f.DisplayPath(), Size: f.Length()}
	}
	if _, err := m.downloads.upsert(ctx, hash, magnet, result); err != nil {
		log.Printf("GetTorrentFiles: failed to write download metadata: %v", err)
	} else {
		m.notifyDownloadsChanged()
		if fileIndex, ok := defaultPreparationFile(result); ok {
			if err := m.StartHLSPreparation(context.Background(), magnet, fileIndex); err != nil {
				log.Printf("GetTorrentFiles: failed to start HLS preparation: %v", err)
			}
		}
	}
	return result, nil
}

func (m *Manager) StartHLSPreparation(ctx context.Context, magnet string, fileIndex int) error {
	_, hash, err := parseMagnet(magnet)
	if err != nil {
		return err
	}
	key := streamKey{InfoHash: hash, Index: fileIndex, Audio: -1, Subtitle: -1}
	operationDone, err := m.reservePreparationOperation(ctx, hash)
	if err != nil {
		return err
	}
	defer m.finishPreparationOperation(hash, operationDone)
	ready, err := streamOutputReady(key.paths(m.cfg.HLSPath))
	if err != nil {
		return err
	}
	if ready {
		if err := m.downloads.selectPrepared(ctx, hash, fileIndex, time.Now()); err != nil {
			return err
		}
		m.notifyDownloadsChanged()
		if err := m.stopSupersededPreparation(context.Background(), key); err != nil {
			return err
		}
		m.cleanupTransientPayload(hash, nil)
		return nil
	}
	download, _, err := m.downloads.beginPreparation(ctx, hash, fileIndex)
	if err != nil {
		return err
	}
	m.notifyDownloadsChanged()
	if err := m.stopSupersededPreparation(context.Background(), key); err != nil {
		return err
	}
	m.launchPreparation(download.Magnet, hash, fileIndex)
	return nil
}

func (m *Manager) reservePreparationOperation(ctx context.Context, hash string) (chan struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.mu.Lock()
		if deletionDone := m.deletions[hash]; deletionDone != nil {
			m.mu.Unlock()
			if err := waitForDone(ctx, deletionDone); err != nil {
				return nil, err
			}
			continue
		}
		if m.preparationOps == nil {
			m.preparationOps = make(map[string]chan struct{})
		}
		if operationDone := m.preparationOps[hash]; operationDone != nil {
			m.mu.Unlock()
			if err := waitForDone(ctx, operationDone); err != nil {
				return nil, err
			}
			continue
		}
		operationDone := make(chan struct{})
		m.preparationOps[hash] = operationDone
		m.mu.Unlock()
		return operationDone, nil
	}
}

func (m *Manager) finishPreparationOperation(hash string, operationDone chan struct{}) {
	m.mu.Lock()
	if m.preparationOps[hash] == operationDone {
		delete(m.preparationOps, hash)
		close(operationDone)
	}
	m.mu.Unlock()
}

func (m *Manager) stopSupersededPreparation(ctx context.Context, keep streamKey) error {
	m.mu.Lock()
	jobs := make([]*preparationJob, 0)
	for key, job := range m.preparations {
		if key.InfoHash == keep.InfoHash && key != keep {
			job.cancel()
			jobs = append(jobs, job)
		}
	}
	m.mu.Unlock()
	for _, job := range jobs {
		if err := waitForDone(ctx, job.done); err != nil {
			return err
		}
	}

	for {
		var (
			key    streamKey
			stream *streamInfo
		)
		m.mu.Lock()
		for candidateKey, candidate := range m.active {
			if candidateKey.InfoHash == keep.InfoHash && candidateKey != keep && !candidate.completed {
				key = candidateKey
				stream = candidate
				break
			}
		}
		m.mu.Unlock()
		if stream == nil {
			return nil
		}
		if err := m.cleanupMatching(ctx, key, stream); err != nil {
			return err
		}
	}
}

func (m *Manager) launchPreparation(magnet, hash string, fileIndex int) {
	current, err := m.downloads.isPreparing(context.Background(), hash, fileIndex)
	if err != nil {
		if !errors.Is(err, ErrDownloadNotFound) {
			log.Printf("failed to validate HLS preparation: hash=%s, file=%d, err=%v", hash, fileIndex, err)
		}
		return
	}
	if !current {
		return
	}
	key := streamKey{InfoHash: hash, Index: fileIndex, Audio: -1, Subtitle: -1}
	ctx, cancel := context.WithCancel(context.Background())
	job := &preparationJob{cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	if m.preparations == nil {
		m.preparations = make(map[streamKey]*preparationJob)
	}
	if m.deletions[hash] != nil || m.preparations[key] != nil || m.active[key] != nil {
		m.mu.Unlock()
		cancel()
		return
	}
	m.preparations[key] = job
	m.mu.Unlock()

	go func() {
		defer cancel()
		defer m.finishPreparationJob(key, job)
		info, err := m.GetMediaInfo(ctx, magnet, fileIndex)
		if err == nil {
			_, err = m.prepareHLSRendition(ctx, magnet, key, info, -1)
			if err == nil {
				ready, readyErr := streamOutputReady(key.paths(m.cfg.HLSPath))
				if readyErr != nil {
					err = readyErr
				} else if ready {
					err = m.downloads.finishPreparation(ctx, hash, fileIndex, time.Now())
					if err == nil {
						m.cleanupTransientPayload(hash, nil)
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Printf("HLS preparation failed: hash=%s, file=%d, err=%v", hash, fileIndex, err)
			m.recordPreparationFailure(hash, fileIndex, err)
			m.cleanupTransientPayload(hash, nil)
		}
		m.notifyDownloadsChanged()
	}()
}

func (m *Manager) finishPreparationJob(key streamKey, job *preparationJob) {
	m.mu.Lock()
	if m.preparations[key] == job {
		delete(m.preparations, key)
	}
	close(job.done)
	m.mu.Unlock()
}

func (m *Manager) recordPreparationFailure(hash string, fileIndex int, preparationErr error) {
	storeErr := m.downloads.failPreparation(context.Background(), hash, fileIndex, preparationErr)
	if storeErr != nil {
		if !errors.Is(storeErr, ErrDownloadNotFound) {
			log.Printf("failed to persist HLS preparation error: hash=%s, file=%d, err=%v", hash, fileIndex, storeErr)
		}
		return
	}
	m.ensureFailedHLSExpiry(hash)
}

func (m *Manager) ensureFailedHLSExpiry(hash string) {
	if !downloadHasReadyHLS(m.cfg.HLSPath, hash) {
		return
	}
	if err := m.downloads.ensureFailedHLSExpiry(context.Background(), hash, time.Now()); err != nil && !errors.Is(err, ErrDownloadNotFound) {
		log.Printf("failed to persist cached HLS expiry: hash=%s, err=%v", hash, err)
	}
}

func (m *Manager) resumePreparations() {
	downloads, err := m.downloads.list(context.Background())
	if err != nil {
		log.Printf("failed to restore HLS preparations: %v", err)
		return
	}
	for _, download := range downloads {
		if download.Status == DownloadStatusExpired {
			continue
		}
		if download.Status == DownloadStatusFailed {
			if err := m.removeIncompleteDownloadHlsDirs(download.ID); err != nil {
				log.Printf("failed to remove incomplete HLS after restart: hash=%s, err=%v", download.ID, err)
			}
			m.ensureFailedHLSExpiry(download.ID)
			m.cleanupTransientPayload(download.ID, nil)
			continue
		}
		if download.Status == DownloadStatusReady && download.SelectedFileIndex != nil {
			if restoreErr := m.StartHLSPreparation(context.Background(), download.Magnet, *download.SelectedFileIndex); restoreErr != nil {
				log.Printf("failed to reconcile ready HLS for %s: %v", download.ID, restoreErr)
			}
			continue
		}
		if download.Status == DownloadStatusPreparing && download.SelectedFileIndex != nil {
			if err := m.resumePreparation(download); err != nil {
				log.Printf("failed to resume HLS preparation for %s: %v", download.ID, err)
			}
			continue
		}
		if download.SelectedFileIndex == nil {
			fileIndex, ok := defaultPreparationFile(download.Files)
			if !ok {
				continue
			}
			if err := m.StartHLSPreparation(context.Background(), download.Magnet, fileIndex); err != nil {
				log.Printf("failed to restore HLS preparation for %s: %v", download.ID, err)
			}
		}
	}
}

func (m *Manager) resumePreparation(download Download) error {
	operationDone, err := m.reservePreparationOperation(context.Background(), download.ID)
	if err != nil {
		return err
	}
	defer m.finishPreparationOperation(download.ID, operationDone)
	if download.SelectedFileIndex == nil {
		return nil
	}
	m.launchPreparation(download.Magnet, download.ID, *download.SelectedFileIndex)
	return nil
}

func (m *Manager) GetMediaInfo(ctx context.Context, magnet string, fileIndex int) (media.MediaInfo, error) {
	_, hash, err := parseMagnet(magnet)
	if err != nil {
		return media.MediaInfo{}, err
	}
	if info, cached, cacheErr := m.downloads.loadMediaInfo(ctx, hash, fileIndex); cacheErr != nil {
		log.Printf("GetMediaInfo: failed to read cached media info: %v", cacheErr)
	} else if cached {
		m.touchDownload(ctx, hash)
		return info, nil
	}

	t, hash, err := m.retainTorrent(ctx, magnet)
	if err != nil {
		return media.MediaInfo{}, err
	}
	defer m.releaseTorrent(hash, t)
	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return media.MediaInfo{}, ctx.Err()
	}
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return media.MediaInfo{}, fmt.Errorf("bad file index")
	}
	m.touchDownload(ctx, hash)
	file := files[fileIndex]
	origPrio := file.Priority()
	file.SetPriority(torrent.PiecePriorityHigh)
	defer file.SetPriority(origPrio)
	file.Download()

	source, err := newTorrentSource(file, m.sources)
	if err != nil {
		return media.MediaInfo{}, err
	}
	defer source.Close()
	info, err := source.Probe(ctx)
	if err != nil {
		return media.MediaInfo{}, err
	}
	if err := m.downloads.saveMediaInfo(ctx, hash, fileIndex, info); err != nil {
		log.Printf("GetMediaInfo: failed to cache media info: %v", err)
	}
	return info, nil
}

func (m *Manager) PrepareHlsStream(ctx context.Context, magnet string, fileIndex, audioTrack, subtitleTrack int) (string, error) {
	_, hash, err := parseMagnet(magnet)
	if err != nil {
		return "", err
	}

	info, err := m.GetMediaInfo(ctx, magnet, fileIndex)
	if err != nil {
		return "", err
	}
	selection := media.StreamSelection{
		AudioTrackIndex:    audioTrack,
		SubtitleTrackIndex: subtitleTrack,
	}
	if err := media.ValidateSelection(info, selection); err != nil {
		return "", err
	}
	bitmapSelected := subtitleTrack >= 0 && media.IsBitmapSubtitle(info.Subtitles[subtitleTrack].Codec)
	if bitmapSelected {
		// Keep the source alive while the shared and burned-in renditions start.
		guard, _, err := m.retainTorrent(ctx, magnet)
		if err != nil {
			return "", err
		}
		defer m.releaseTorrent(hash, guard)
	}

	baseKey := streamKey{InfoHash: hash, Index: fileIndex, Audio: -1, Subtitle: -1}
	if _, err := m.prepareHLSRendition(ctx, magnet, baseKey, info, -1); err != nil {
		return "", err
	}

	videoKey := baseKey
	if bitmapSelected {
		videoKey.Subtitle = subtitleTrack
		if _, err := m.prepareHLSRendition(ctx, magnet, videoKey, info, subtitleTrack); err != nil {
			return "", err
		}
	}

	basePaths := baseKey.paths(m.cfg.HLSPath)
	videoPaths := videoKey.paths(m.cfg.HLSPath)
	masterPlaylist := videoPaths.selectionMaster(audioTrack, subtitleTrack)
	if err := media.WriteMasterPlaylist(
		masterPlaylist,
		videoPaths.videoPlaylist,
		basePaths.outDir,
		info,
		selection,
	); err != nil {
		return "", fmt.Errorf("write selection master: %w", err)
	}
	m.touchDownload(ctx, hash)
	log.Printf("Stream ready: key=%v, playlist=%s", videoKey, masterPlaylist)
	return masterPlaylist, nil
}

func (m *Manager) prepareHLSRendition(
	ctx context.Context,
	magnet string,
	key streamKey,
	info media.MediaInfo,
	bitmapSubtitle int,
) (string, error) {
	paths := key.paths(m.cfg.HLSPath)

	if s, err := m.getStream(ctx, key); err != nil {
		return "", err
	} else if s != nil {
		m.touchDownload(ctx, key.InfoHash)
		return m.waitForPlayableStream(ctx, s)
	}
	if ready, err := m.activateCachedStream(ctx, key, paths); err != nil {
		return "", err
	} else if ready {
		m.touchDownload(ctx, key.InfoHash)
		log.Printf("Reusing completed HLS rendition: key=%v, playlist=%s", key, paths.masterPlaylist)
		return paths.masterPlaylist, nil
	}
	// A concurrent request may have created the rendition while the ready output was checked.
	if s, err := m.getStream(ctx, key); err != nil {
		return "", err
	} else if s != nil {
		m.touchDownload(ctx, key.InfoHash)
		return m.waitForPlayableStream(ctx, s)
	}

	t, _, err := m.retainTorrent(ctx, magnet)
	if err != nil {
		log.Printf("prepareHLSRendition: AddMagnet failed: %v", err)
		return "", err
	}
	torrentRetained := true
	defer func() {
		if torrentRetained {
			m.releaseTorrent(key.InfoHash, t)
		}
	}()

	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return "", ctx.Err()
	}
	files := t.Files()
	if key.Index < 0 || key.Index >= len(files) {
		log.Printf("prepareHLSRendition: bad file index: %d", key.Index)
		return "", fmt.Errorf("bad file index")
	}
	file := files[key.Index]
	m.touchDownload(ctx, key.InfoHash)

	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if s, err := m.getStream(ctx, key); err != nil {
			return "", err
		} else if s != nil {
			return m.waitForPlayableStream(ctx, s)
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
			cancel:         cancel,
			torrent:        t,
			file:           file,
			lastView:       time.Now(),
			paths:          paths,
			source:         source,
			mediaInfo:      info,
			bitmapSubtitle: bitmapSubtitle,
			playable:       make(chan struct{}),
			runDone:        make(chan struct{}),
		}
		m.mu.Lock()
		m.active[key] = s
		torrentRetained = false
		m.mu.Unlock()

		file.Download()
		file.SetPriority(torrent.PiecePriorityHigh)
		source.PrefetchRange(0, initialProbeBytes)

		m.launchConversion(streamCtx, key, s)
		m.finishStreamOperation(key, operationDone)
		m.notifyDownloadsChanged()
		playlist, err := m.waitForPlayableStream(ctx, s)
		if err != nil {
			return "", err
		}
		log.Printf("HLS rendition ready: key=%v, playlist=%s", key, paths.masterPlaylist)
		return playlist, nil
	}
}

func (m *Manager) activateCachedStream(ctx context.Context, key streamKey, paths streamPaths) (bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		m.mu.Lock()
		if deletionDone := m.deletions[key.InfoHash]; deletionDone != nil {
			m.mu.Unlock()
			if err := waitForDone(ctx, deletionDone); err != nil {
				return false, err
			}
			continue
		}
		if operationDone := m.streamOps[key]; operationDone != nil {
			m.mu.Unlock()
			if err := waitForDone(ctx, operationDone); err != nil {
				return false, err
			}
			continue
		}
		if _, exists := m.active[key]; exists {
			m.mu.Unlock()
			return false, nil
		}
		operationDone := m.reserveStreamOperationLocked(key)
		m.mu.Unlock()

		ready, err := streamOutputReady(paths)
		if err != nil {
			m.finishStreamOperation(key, operationDone)
			return false, err
		}
		if !ready {
			m.finishStreamOperation(key, operationDone)
			return false, nil
		}

		s := &streamInfo{
			lastView:  time.Now(),
			paths:     paths,
			completed: true,
		}
		m.mu.Lock()
		if deletionDone := m.deletions[key.InfoHash]; deletionDone != nil {
			m.mu.Unlock()
			m.finishStreamOperation(key, operationDone)
			if err := waitForDone(ctx, deletionDone); err != nil {
				return false, err
			}
			continue
		}
		m.active[key] = s
		m.mu.Unlock()
		m.finishStreamOperation(key, operationDone)
		return true, nil
	}
}

func (m *Manager) getStream(ctx context.Context, key streamKey) (*streamInfo, error) {
	for {
		m.mu.Lock()
		if deletionDone := m.deletions[key.InfoHash]; deletionDone != nil {
			m.mu.Unlock()
			if err := waitForDone(ctx, deletionDone); err != nil {
				return nil, err
			}
			continue
		}
		if operationDone := m.streamOps[key]; operationDone != nil {
			m.mu.Unlock()
			if err := waitForDone(ctx, operationDone); err != nil {
				return nil, err
			}
			continue
		}

		s := m.active[key]
		if s != nil {
			s.mtx.Lock()
			s.lastView = time.Now()
			s.mtx.Unlock()
		}
		m.mu.Unlock()
		return s, nil
	}
}

func (m *Manager) waitForPlayableStream(ctx context.Context, s *streamInfo) (string, error) {
	if err := s.waitPlayable(ctx); err != nil {
		return "", err
	}
	return s.paths.masterPlaylist, nil
}

func (m *Manager) ListDownloads(ctx context.Context) ([]Download, error) {
	downloads, err := m.downloads.list(ctx)
	if err != nil {
		return nil, err
	}
	hlsSizes := hlsDiskSizes(m.cfg.HLSPath)
	for i := range downloads {
		downloads[i].DiskSize += hlsSizes[downloads[i].ID]
	}
	return downloads, nil
}

func (m *Manager) ExtendDownload(ctx context.Context, id string, extension time.Duration) (Download, error) {
	download, err := m.downloads.extend(ctx, id, extension)
	if err != nil {
		return Download{}, err
	}
	download.DiskSize += hlsDiskSize(m.cfg.HLSPath, download.ID)
	m.notifyDownloadsChanged()
	return download, nil
}

func (m *Manager) DeleteDownload(ctx context.Context, id string) error {
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

	if err := m.cancelPreparations(ctx, id); err != nil {
		return err
	}
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

func (m *Manager) cancelPreparations(ctx context.Context, id string) error {
	m.mu.Lock()
	jobs := make([]*preparationJob, 0)
	for key, job := range m.preparations {
		if key.InfoHash == id {
			job.cancel()
			jobs = append(jobs, job)
		}
	}
	m.mu.Unlock()
	for _, job := range jobs {
		if err := waitForDone(ctx, job.done); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) touchDownload(ctx context.Context, id string) {
	if err := m.downloads.touch(ctx, id); err != nil && !errors.Is(err, ErrDownloadNotFound) {
		log.Printf("failed to touch download metadata: %v", err)
	} else if err == nil {
		m.notifyDownloadsChanged()
	}
}

func (m *Manager) SubscribeDownloadEvents(ctx context.Context) <-chan struct{} {
	return m.events.subscribe(ctx)
}

func (m *Manager) notifyDownloadsChanged() {
	if m.events == nil {
		return
	}
	m.events.notify()
}

func (m *Manager) streamKeysForDownload(id string) []streamKey {
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

func (m *Manager) downloadActive(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.active {
		if key.InfoHash == id {
			return true
		}
	}
	return false
}

func (m *Manager) cleanupExpiredDownloads(ctx context.Context) {
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

func (m *Manager) removeDownloadHlsDirs(id string) error {
	entries, err := os.ReadDir(m.cfg.HLSPath)
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
		if err := os.RemoveAll(filepath.Join(m.cfg.HLSPath, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) removeIncompleteDownloadHlsDirs(id string) error {
	entries, err := os.ReadDir(m.cfg.HLSPath)
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
		path := filepath.Join(m.cfg.HLSPath, entry.Name())
		key, err := parseStreamDir(entry.Name())
		if err != nil {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			continue
		}

		m.mu.Lock()
		if m.deletions[id] != nil || m.preparations[key] != nil || m.active[key] != nil || m.streamOps[key] != nil {
			m.mu.Unlock()
			continue
		}
		operationDone := m.reserveStreamOperationLocked(key)
		m.mu.Unlock()

		cleanupErr := func() error {
			defer m.finishStreamOperation(key, operationDone)
			ready, err := streamOutputReady(key.paths(m.cfg.HLSPath))
			if err != nil || ready {
				return err
			}
			return os.RemoveAll(path)
		}()
		if cleanupErr != nil {
			return cleanupErr
		}
	}
	return nil
}

func hlsDiskSizes(root string) map[string]int64 {
	sizes := make(map[string]int64)
	entries, err := os.ReadDir(root)
	if err != nil {
		return sizes
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		rawID, _, found := strings.Cut(entry.Name(), "_")
		id, err := cleanInfoHash(rawID)
		if !found || err != nil {
			continue
		}
		sizes[id] += pathDiskSize(filepath.Join(root, entry.Name()))
	}
	return sizes
}

func downloadHasReadyHLS(root, id string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		key, err := parseStreamDir(entry.Name())
		if err != nil || key.InfoHash != id {
			continue
		}
		ready, err := streamOutputReady(key.paths(root))
		if err == nil && ready {
			return true
		}
	}
	return false
}

func defaultPreparationFile(files []FileInfo) (int, bool) {
	selected := -1
	for _, file := range files {
		switch strings.ToLower(filepath.Ext(file.Name)) {
		case ".avi", ".flv", ".m2ts", ".m4v", ".mkv", ".mov", ".mp4", ".mpeg", ".mpg", ".ogv", ".ts", ".webm", ".wmv":
			if selected >= 0 {
				return 0, false
			}
			selected = file.Index
		}
	}
	if selected >= 0 {
		return selected, true
	}
	if len(files) == 1 {
		return files[0].Index, true
	}
	return 0, false
}

func hlsDiskSize(root, id string) int64 {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	prefix := id + "_"
	var total int64
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			total += pathDiskSize(filepath.Join(root, entry.Name()))
		}
	}
	return total
}

func pathDiskSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err == nil {
			total += allocatedFileSize(info)
		}
		return nil
	})
	return total
}

func (m *Manager) launchConversion(
	streamCtx context.Context,
	key streamKey,
	s *streamInfo,
) {
	runDone := s.runDone
	go func() {
		err := m.runConversion(streamCtx, s)
		if err == nil {
			err = markStreamOutputReady(s.paths)
		}
		if err == nil {
			m.finishConversion(key, s, nil)
			close(runDone)
			return
		}
		close(runDone)
		m.finishConversion(key, s, err)
	}()
}

func (m *Manager) runConversion(
	streamCtx context.Context,
	s *streamInfo,
) error {
	if err := s.source.WaitRange(streamCtx, 0, initialProbeBytes); err != nil {
		s.signalPlayable(err)
		return err
	}

	conversionCtx, cancelConversion := context.WithCancel(streamCtx)
	defer cancelConversion()
	converter := media.NewURLConverter(
		conversionCtx,
		s.source.URL(),
		s.paths.outDir,
		s.paths.videoPlaylist,
		s.paths.masterPlaylist,
		s.mediaInfo,
		s.bitmapSubtitle,
	)
	errCh := make(chan error, 1)
	go func() {
		errCh <- converter.ConvertToHLS()
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
				s.signalPlayable(err)
			}
			return err
		case <-streamDone:
			err := streamCtx.Err()
			if !playableSent {
				s.signalPlayable(err)
			}
			cancelConversion()
			<-errCh
			return err
		case err := <-playlistReadyCh:
			if err != nil {
				if !playableSent {
					s.signalPlayable(err)
				}
				cancelConversion()
				<-errCh
				return err
			}
			s.signalPlayable(nil)
			playableSent = true
			streamDone = nil
			playlistReadyCh = nil
		}
	}
}

func (m *Manager) finishConversion(key streamKey, s *streamInfo, err error) {
	var (
		cancel context.CancelFunc
		source *torrentSource
		t      *torrent.Torrent
	)
	m.mu.Lock()
	if current, ok := m.active[key]; !ok || current != s {
		m.mu.Unlock()
		return
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("Stream conversion error for key=%v: %v", key, err)
	}
	notifyCompleted := false
	if err == nil {
		s.completed = true
		cancel = s.cancel
		s.cancel = nil
		source = s.source
		s.source = nil
		t = s.torrent
		s.torrent = nil
		s.file = nil
		notifyCompleted = true
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if source != nil {
		source.Close()
	}
	if err == nil && key.Audio == -1 && key.Subtitle == -1 && m.downloads != nil {
		if storeErr := m.downloads.finishPreparation(context.Background(), key.InfoHash, key.Index, time.Now()); storeErr != nil {
			log.Printf("failed to persist completed HLS preparation: key=%v, err=%v", key, storeErr)
		}
	}
	if t != nil {
		m.releaseTorrent(key.InfoHash, t)
	}
	if notifyCompleted {
		m.notifyDownloadsChanged()
	}

	if err != nil {
		if key.Audio == -1 && key.Subtitle == -1 && m.downloads != nil {
			m.recordPreparationFailure(key.InfoHash, key.Index, err)
			m.notifyDownloadsChanged()
		}
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
