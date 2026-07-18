package torrent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cinemator/application"
	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
)

const (
	streamInitializationTimeout = 10 * time.Minute
	windowGenerationTimeout     = 10 * time.Minute
	hlsTouchInterval            = time.Minute
)

var (
	errStreamJobQueueFull = errors.New("stream job queue is full")
	errStreamJobLimit     = errors.New("stream has too many pending jobs")
)

func (s *streamInfo) waitReady(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-s.ready:
		s.mtx.Lock()
		err := s.readyErr
		s.mtx.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *manager) initializeOnDemandStream(key streamKey, s *streamInfo) {
	readyPublished := false
	defer func() {
		if recovered := recover(); recovered != nil {
			err := fmt.Errorf("stream initialization panic: %v", recovered)
			if readyPublished {
				log.Printf("On-demand HLS background initialization panicked for key=%v: %v", key, err)
				return
			}
			s.mtx.Lock()
			s.readyErr = err
			s.fatalErr = err
			s.status.Phase = "error"
			s.status.Message = publicStreamError(err)
			s.status.LastProgress = time.Now()
			close(s.ready)
			readyPublished = true
			s.mtx.Unlock()
			log.Printf("Prepare on-demand HLS panicked for key=%v: %v", key, err)
		}
	}()
	initCtx, cancelInit := context.WithTimeout(s.ctx, streamInitializationTimeout)
	defer cancelInit()
	s.mtx.Lock()
	info := s.mediaInfo
	infoReady := s.mediaInfoReady
	s.mtx.Unlock()
	var err error
	if !infoReady {
		info, err = s.source.Probe(initCtx)
	}
	if err == nil {
		if info.VideoCodec == "" {
			err = fmt.Errorf("%w: selected file has no video stream", domain.ErrHlsAssetUnsupported)
		}
	}
	if err == nil && !infoReady {
		m.mu.Lock()
		m.mediaInfo[mediaKey{InfoHash: key.InfoHash, Index: key.Index}] = info
		m.mu.Unlock()
	}
	if err == nil {
		info.Seekable = info.Duration > 0
		if info.Duration > 0 && s.presentationTarget >= info.Duration {
			err = fmt.Errorf("%w: start position is outside media duration", domain.ErrBadHlsRequest)
		}
	}
	directPlay := err == nil && ffmpeg.CanRemuxHLS(info, s.selection)
	if err == nil {
		err = func() error {
			s.playlistMtx.Lock()
			defer s.playlistMtx.Unlock()
			var reservation *diskReservation
			if m.hlsDisk != nil {
				var reserveErr error
				reservation, reserveErr = m.hlsDisk.Reserve(estimatedHlsMetadataBytes(info.Duration, m.settings.HlsSegmentDuration(), s.selection.SubtitleTrackIndex >= 0), 8)
				if reserveErr != nil {
					return reserveErr
				}
				defer reservation.Release()
			}
			return ffmpeg.PrepareOnDemandHLS(
				s.paths.outDir,
				s.paths.videoPlaylist,
				s.paths.subtitlePlaylist,
				s.paths.masterPlaylist,
				info,
				s.selection,
				m.settings.HlsSegmentDuration(),
				m.settings.HlsWindowSegments(),
				s.assetVersion,
			)
		}()
	}

	s.mtx.Lock()
	s.mediaInfo = info
	s.mediaInfoReady = err == nil
	s.directPlay = directPlay
	s.readyErr = err
	if err != nil {
		s.fatalErr = err
		s.status.Phase = "error"
		if s.status.Message == "" {
			s.status.Message = publicStreamError(err)
		}
	} else {
		s.status.Phase = "waiting"
		s.status.Mode = ffmpeg.HLSMode(info, s.selection)
		s.status.Seekable = info.Seekable
		s.status.Duration = info.Duration
	}
	s.status.LastProgress = time.Now()
	close(s.ready)
	readyPublished = true
	s.mtx.Unlock()
	if err != nil {
		log.Printf("Prepare on-demand HLS failed for key=%v: %v", key, err)
		return
	}
	m.prefetchProgressiveWindow(s, -1)
}

func estimatedHlsMetadataBytes(duration float64, segmentDuration time.Duration, subtitles bool) uint64 {
	const base = uint64(1 << 20)
	if duration <= 0 || segmentDuration <= 0 {
		return base
	}
	entries := math.Ceil(duration / segmentDuration.Seconds())
	if subtitles {
		entries *= 2
	}
	if entries >= float64((math.MaxInt64-base)/128) {
		return math.MaxInt64
	}
	return base + uint64(entries)*128
}

func (m *manager) OpenHlsAsset(ctx context.Context, streamDir, assetName, version string) (application.HlsAsset, error) {
	if assetName == "" || filepath.Base(assetName) != assetName {
		return application.HlsAsset{}, fmt.Errorf("%w: bad asset name", domain.ErrBadHlsRequest)
	}
	key, err := parseStreamDir(streamDir)
	if err != nil {
		return application.HlsAsset{}, fmt.Errorf("%w: bad stream: %v", domain.ErrBadHlsRequest, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := m.ensureHlsAsset(ctx, streamDir, assetName, version); err != nil {
			return application.HlsAsset{}, err
		}
		m.mu.Lock()
		stream := m.active[key]
		m.mu.Unlock()
		if stream == nil {
			return application.HlsAsset{}, domain.ErrHlsStreamNotFound
		}
		stream.generationMtx.RLock()
		var unlockPlaylist func()
		if strings.HasSuffix(assetName, ".m3u8") {
			stream.playlistMtx.RLock()
			unlockPlaylist = stream.playlistMtx.RUnlock
		}
		stream.mtx.Lock()
		currentVersion := stream.assetVersion
		stream.mtx.Unlock()
		if assetName != "master.m3u8" && version != currentVersion {
			if unlockPlaylist != nil {
				unlockPlaylist()
			}
			stream.generationMtx.RUnlock()
			return application.HlsAsset{}, domain.ErrHlsPlaylistChanged
		}
		asset, err := m.assets.Open(filepath.Join(m.settings.HlsPath(), streamDir, assetName))
		stream.generationMtx.RUnlock()
		if err == nil {
			if unlockPlaylist != nil {
				asset.ReadSeekCloser = &lockedHlsAsset{ReadSeekCloser: asset.ReadSeekCloser, unlock: unlockPlaylist}
			}
			return asset, nil
		}
		if unlockPlaylist != nil {
			unlockPlaylist()
		}
		if !errors.Is(err, os.ErrNotExist) {
			return application.HlsAsset{}, err
		}
	}
	return application.HlsAsset{}, os.ErrNotExist
}

type lockedHlsAsset struct {
	application.ReadSeekCloser
	unlock func()
	once   sync.Once
	err    error
}

func (a *lockedHlsAsset) Close() error {
	a.once.Do(func() {
		defer a.unlock()
		a.err = a.ReadSeekCloser.Close()
	})
	return a.err
}

// evictActiveHlsAssets rotates one presentation generation and removes the
// already selected assets. The caller holds generationMtx from CanEvict through
// this call so no HTTP open can invalidate that selection.
func (m *manager) evictActiveHlsAssets(stream *streamInfo, items []hlsCacheItem) (map[string]struct{}, error) {
	removed := make(map[string]struct{}, len(items))
	if len(items) == 0 {
		return removed, nil
	}

	stream.playlistMtx.Lock()
	defer stream.playlistMtx.Unlock()
	evictedOwners := make(map[int]struct{})
	for _, item := range items {
		name := filepath.Base(item.path)
		if owner, ok := parseDirectSegmentOwner(name); ok {
			evictedOwners[owner] = struct{}{}
		}
		if owner, ok := parseDirectInitOwner(name); ok {
			evictedOwners[owner] = struct{}{}
		}
		if index, ok := parseSegmentName(name, "chunk_", ".ts"); ok {
			owner, _ := segmentWindow(index, 0, m.settings.HlsWindowSegments())
			evictedOwners[owner] = struct{}{}
		}
	}
	stream.mtx.Lock()
	if stream.closing || stream.fatalErr != nil || stream.assetVersion == "" {
		stream.mtx.Unlock()
		return removed, nil
	}
	version := fmt.Sprintf("%x", time.Now().UnixNano())
	info := stream.mediaInfo
	selection := stream.selection
	directPlay := stream.directPlay
	presentationTarget := stream.presentationTarget
	materializedTarget := stream.materializedTarget
	if materializedTarget <= 0 {
		materializedTarget = 2 * m.settings.HlsSegmentDuration()
	}
	progressiveCount := stream.progressiveAdvertised
	progressiveSubtitles := stream.progressiveSubtitles
	progressiveSequence := 0
	progressiveDiscontinuitySequence := 0
	progressiveLast := stream.progressiveLast
	progressiveEnded := stream.progressiveEnded
	directWindows := make(map[int][]ffmpeg.HLSFragment, len(stream.directWindows))
	fragments := make([]ffmpeg.HLSFragment, 0)
	for owner, window := range stream.directWindows {
		if _, evicted := evictedOwners[owner]; evicted {
			continue
		}
		directWindows[owner] = window
		fragments = append(fragments, window...)
	}
	stream.mtx.Unlock()
	var reservation *diskReservation
	var err error
	if m.hlsDisk != nil {
		reservation, err = m.hlsDisk.Reserve(estimatedHlsMetadataBytes(info.Duration, m.settings.HlsSegmentDuration(), selection.SubtitleTrackIndex >= 0), 4)
		if err != nil {
			stream.mtx.Lock()
			stream.fatalErr = err
			stream.status.Phase = "error"
			stream.status.Message = publicStreamError(err)
			stream.status.LastProgress = time.Now()
			stream.mtx.Unlock()
			return removed, err
		}
		defer reservation.Release()
	}

	err = ffmpeg.PrepareOnDemandHLS(
		stream.paths.outDir,
		stream.paths.videoPlaylist,
		stream.paths.subtitlePlaylist,
		stream.paths.masterPlaylist,
		info,
		selection,
		m.settings.HlsSegmentDuration(),
		m.settings.HlsWindowSegments(),
		version,
	)
	if err == nil {
		err = ffmpeg.UpdateMaterializedHLS(
			stream.paths.videoPlaylist,
			materializedTarget,
			version,
			directPlay && ffmpeg.UsesFMP4(info, selection),
			progressiveSequence,
			progressiveDiscontinuitySequence,
			presentationTarget,
			progressiveEnded,
			fragments,
		)
		if err == nil && ffmpeg.UsesTextSubtitles(info, selection) {
			err = ffmpeg.UpdateProgressiveSubtitleHLS(
				stream.paths.subtitlePlaylist,
				m.settings.HlsSegmentDuration(),
				m.settings.HlsWindowSegments(),
				version,
				progressiveSubtitles,
				progressiveLast,
				progressiveEnded && progressiveSubtitles >= progressiveCount,
			)
		}
	}
	stream.mtx.Lock()
	if err != nil {
		stream.fatalErr = fmt.Errorf("rotate HLS generation: %w", err)
		stream.status.Phase = "error"
		stream.status.Message = publicStreamError(stream.fatalErr)
		stream.status.LastProgress = time.Now()
		stream.mtx.Unlock()
		return removed, stream.fatalErr
	}
	stream.directWindows = directWindows
	stream.progressiveSequence = progressiveSequence
	stream.progressiveDiscontinuitySequence = progressiveDiscontinuitySequence
	stream.materializedTarget = materializedTarget
	stream.assetVersion = version
	stream.mtx.Unlock()

	for _, item := range items {
		ok, removeErr := m.assets.TryEvict(item.path)
		if removeErr != nil {
			return removed, removeErr
		}
		if ok {
			removed[item.path] = struct{}{}
		}
	}
	return removed, nil
}

func (m *manager) ensureHlsAsset(ctx context.Context, streamDir, assetName, version string) error {
	key, err := parseStreamDir(streamDir)
	if err != nil {
		return fmt.Errorf("%w: bad stream: %v", domain.ErrBadHlsRequest, err)
	}
	m.mu.Lock()
	s, ok := m.active[key]
	m.mu.Unlock()
	if !ok {
		return domain.ErrHlsStreamNotFound
	}
	if err := s.waitReady(ctx); err != nil {
		return err
	}

	s.playlistMtx.RLock()
	s.mtx.Lock()
	s.lastView = time.Now()
	fatalErr := s.fatalErr
	closing := s.closing
	currentVersion := s.assetVersion
	s.mtx.Unlock()
	s.playlistMtx.RUnlock()
	if closing {
		return context.Canceled
	}
	if fatalErr != nil {
		return fatalErr
	}
	if assetName != "master.m3u8" && version != currentVersion {
		return domain.ErrHlsPlaylistChanged
	}
	if strings.HasSuffix(assetName, ".m3u8") {
		if assetName == "subs.m3u8" {
			s.mtx.Lock()
			begin, end := s.progressiveSubtitles, s.progressiveAdvertised
			s.mtx.Unlock()
			m.prefetchProgressiveSubtitles(s, begin, end)
		}
		return nil
	}
	owner, directAsset := parseDirectSegmentOwner(assetName)
	if !directAsset {
		owner, directAsset = parseDirectInitOwner(assetName)
	}
	if directAsset {
		path := filepath.Join(s.paths.outDir, assetName)
		if touchExisting(path) {
			m.prefetchProgressiveWindow(s, owner)
			return nil
		}
		s.mtx.Lock()
		delete(s.directWindows, owner)
		s.mtx.Unlock()
		if err := m.ensureVideoSegment(ctx, s, owner); err != nil {
			return err
		}
		if !touchExisting(path) {
			return domain.ErrHlsPlaylistChanged
		}
		m.prefetchProgressiveWindow(s, owner)
		return nil
	}
	if index, ok := parseSegmentName(assetName, "chunk_", ".ts"); ok {
		path := filepath.Join(s.paths.outDir, assetName)
		if touchExisting(path) {
			m.prefetchProgressiveWindow(s, index)
			return nil
		}
		s.mtx.Lock()
		begin, _ := segmentWindow(index, 0, m.settings.HlsWindowSegments())
		delete(s.directWindows, begin)
		s.mtx.Unlock()
		if err := m.ensureVideoSegment(ctx, s, index); err != nil {
			return err
		}
		if !touchExisting(path) {
			return domain.ErrHlsPlaylistChanged
		}
		m.prefetchProgressiveWindow(s, index)
		return nil
	}
	if index, ok := parseSegmentName(assetName, "subs_", ".vtt"); ok {
		return m.ensureSubtitleSegment(ctx, s, index)
	}
	return domain.ErrHlsAssetUnsupported
}

func (m *manager) ensureVideoSegment(ctx context.Context, s *streamInfo, index int) error {
	s.mtx.Lock()
	closing := s.closing
	fatalErr := s.fatalErr
	directPlay := s.directPlay
	s.mtx.Unlock()
	if closing {
		return context.Canceled
	}
	if fatalErr != nil {
		return fatalErr
	}
	path := filepath.Join(s.paths.outDir, videoSegmentName(index))
	if touchExisting(path) {
		s.markPreparing(index, m.settings.HlsSegmentDuration())
		s.markReady(index)
		m.prefetchProgressiveWindow(s, index)
		return nil
	}

	s.mtx.Lock()
	if s.closing {
		s.mtx.Unlock()
		return context.Canceled
	}
	if s.fatalErr != nil {
		err := s.fatalErr
		s.mtx.Unlock()
		return err
	}
	total := 0
	if s.mediaInfo.Duration > 0 {
		total = segmentCount(s.mediaInfo.Duration, m.settings.HlsSegmentDuration())
		if index < 0 || index >= total {
			s.mtx.Unlock()
			return errors.New("video segment out of range")
		}
	} else {
		if index < 0 || index >= s.progressiveAdvertised {
			s.mtx.Unlock()
			return errors.New("video segment is outside the discovered range")
		}
	}
	s.mtx.Unlock()

	s.markPreparing(index, m.settings.HlsSegmentDuration())
	s.mtx.Lock()
	if s.closing {
		s.mtx.Unlock()
		return context.Canceled
	}
	if s.fatalErr != nil {
		err := s.fatalErr
		s.mtx.Unlock()
		return err
	}
	if directPlay {
		begin, _ := segmentWindow(index, total, m.settings.HlsWindowSegments())
		if len(s.directWindows[begin]) > 0 {
			s.mtx.Unlock()
			s.markReady(index)
			return nil
		}
	}
	job := findSegmentJob(s.videoJobs, index)
	if job == nil {
		if total <= 0 {
			total = s.progressiveAdvertised
		}
		begin, end := segmentWindow(index, total, m.settings.HlsWindowSegments())
		cancelAbandonedJobsLocked(s.videoJobs, begin, end)
		if err := m.reserveJobSlotLocked(s); err != nil {
			s.mtx.Unlock()
			s.markSegmentError(index, err)
			return fmt.Errorf("%w: %v", domain.ErrHlsTemporarilyUnavailable, err)
		}
		jobCtx, cancel := context.WithCancel(s.ctx)
		now := time.Now()
		job = &segmentJob{begin: begin, end: end, id: segmentJobID("video", begin, end, now), cancel: cancel, done: make(chan struct{}), startedAt: now, lastProgress: now, slotHeld: true}
		if s.videoJobs == nil {
			s.videoJobs = make(map[*segmentJob]struct{})
		}
		s.videoJobs[job] = struct{}{}
		go m.runVideoJob(s, jobCtx, job)
	}
	job.waiters++
	s.mtx.Unlock()
	var err error
	if directPlay {
		err = waitForJob(ctx, job)
	} else {
		err = waitForGeneratedAsset(ctx, path, job)
	}
	m.releaseJobWaiter(s, job, err)
	if err == nil {
		s.markReady(index)
		m.prefetchProgressiveWindow(s, index)
	}
	return err
}

// prefetchProgressiveWindow keeps one small, fully generated window ahead of
// playback. Unknown-duration EVENT playlists can then grow without advertising
// files that do not exist or requiring a guessed final duration.
func (m *manager) prefetchProgressiveWindow(s *streamInfo, requested int) {
	window := m.settings.HlsWindowSegments()
	s.mtx.Lock()
	if s.closing || s.progressiveEnded ||
		s.progressiveRetry || (requested >= 0 && requested < max(0, s.progressiveAdvertised-window)) {
		s.mtx.Unlock()
		return
	}
	begin := s.progressiveAdvertised
	if s.directPlay {
		begin = max(begin, nextDirectWindowBegin(s.directWindows, m.settings.HlsSegmentDuration()))
	}
	total := 0
	if s.mediaInfo.Duration > 0 {
		total = segmentCount(s.mediaInfo.Duration, m.settings.HlsSegmentDuration())
		if begin >= total {
			s.progressiveEnded = true
			s.mtx.Unlock()
			return
		}
	}
	if findSegmentJob(s.videoJobs, begin) != nil {
		s.mtx.Unlock()
		return
	}
	if err := m.reserveJobSlotLocked(s); err != nil {
		s.progressiveRetry = true
		s.status.Phase = "waiting"
		s.status.Message = "The server transcode queue is busy; retrying"
		s.status.LastProgress = time.Now()
		s.mtx.Unlock()
		go m.retryProgressivePrefetch(s)
		return
	}
	jobCtx, cancel := context.WithCancel(s.ctx)
	now := time.Now()
	end := begin + window
	if total > 0 {
		end = min(end, total)
	}
	job := &segmentJob{begin: begin, end: end, id: segmentJobID("video", begin, end, now), cancel: cancel, done: make(chan struct{}), startedAt: now, lastProgress: now, background: true, slotHeld: true}
	if s.videoJobs == nil {
		s.videoJobs = make(map[*segmentJob]struct{})
	}
	s.videoJobs[job] = struct{}{}
	s.mtx.Unlock()
	if requested < 0 {
		s.markPreparing(begin, m.settings.HlsSegmentDuration())
	}
	go m.runVideoJob(s, jobCtx, job)
}

func (m *manager) retryProgressivePrefetch(s *streamInfo) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return
	case <-timer.C:
	}
	s.mtx.Lock()
	s.progressiveRetry = false
	closing := s.closing
	s.mtx.Unlock()
	if !closing {
		m.prefetchProgressiveWindow(s, -1)
	}
}

func (m *manager) prefetchProgressiveSubtitles(s *streamInfo, begin, end int) {
	s.mtx.Lock()
	if s.closing || !ffmpeg.UsesTextSubtitles(s.mediaInfo, s.selection) {
		s.mtx.Unlock()
		return
	}
	begin = min(begin, s.progressiveSubtitles)
	end = min(end, s.progressiveAdvertised)
	end = min(end, begin+m.settings.HlsWindowSegments())
	if begin >= end || findSegmentJob(s.subtitleJobs, begin) != nil {
		s.mtx.Unlock()
		return
	}
	if err := m.reserveJobSlotLocked(s); err != nil {
		s.mtx.Unlock()
		return
	}
	jobCtx, cancel := context.WithCancel(s.ctx)
	now := time.Now()
	job := &segmentJob{
		begin: begin, end: end,
		id:     segmentJobID("subtitle", begin, end, now),
		cancel: cancel, done: make(chan struct{}),
		startedAt: now, lastProgress: now,
		background: true, slotHeld: true,
	}
	if s.subtitleJobs == nil {
		s.subtitleJobs = make(map[*segmentJob]struct{})
	}
	s.subtitleJobs[job] = struct{}{}
	s.mtx.Unlock()
	go m.runSubtitleJob(s, jobCtx, job)
}

func (m *manager) runVideoJob(s *streamInfo, ctx context.Context, job *segmentJob) {
	defer func() {
		if recovered := recover(); recovered != nil {
			job.err = fmt.Errorf("video generation panic: %v", recovered)
		}
		if errors.Is(job.err, context.Canceled) {
			s.markJobCanceled(job)
		} else {
			s.markJobError(job, job.err)
		}
		close(job.done)
		s.mtx.Lock()
		delete(s.videoJobs, job)
		s.mtx.Unlock()
		m.releaseJobSlot(job)
		m.enforceCacheLimit()
	}()
	ctx, cancel := context.WithTimeout(ctx, windowGenerationTimeout)
	defer cancel()
	s.mtx.Lock()
	info := s.mediaInfo
	selection := s.selection
	directPlay := s.directPlay
	s.mtx.Unlock()

	log.Printf("Generating HLS video window: dir=%s, segments=[%d,%d) mode=%s", filepath.Base(s.paths.outDir), job.begin, job.end, ffmpeg.HLSMode(info, selection))
	segmentCount := job.end - job.begin
	generationDuration := time.Duration(segmentCount) * m.settings.HlsSegmentDuration()
	bitrate := ffmpeg.HLSReservationBitrate(info, selection)
	prerollBudget := m.directPrerollBudget(bitrate)
	if directPlay {
		generationDuration = ffmpeg.DirectWindowGenerationDuration(segmentCount, m.settings.HlsSegmentDuration(), prerollBudget)
	}
	release, err := m.reserveHlsGeneration(generationDuration, bitrate)
	if err != nil {
		job.err = err
		return
	}
	defer func() { release() }()
	ctx, stopResourceGuard := context.WithCancelCause(ctx)
	resourceGuard := m.monitorHlsResources(ctx, stopResourceGuard)
	defer func() {
		stopResourceGuard(context.Canceled)
		resourceErr := <-resourceGuard
		if cause := context.Cause(ctx); resourceErr == nil && cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
			resourceErr = cause
		}
		if resourceErr != nil && (job.err == nil || errors.Is(job.err, context.Canceled)) {
			job.err = resourceErr
		}
	}()
	s.markJobStarted(job)

	generateTranscoded := func() error {
		job.result, job.err = ffmpeg.GenerateVideoWindow(
			ctx,
			s.source.URLForJob(job.id),
			s.paths.outDir,
			info,
			selection,
			job.begin,
			job.end-job.begin,
			m.settings.HlsSegmentDuration(),
			func(index int) error {
				s.markSegmentProgress(job, index)
				if err := m.checkHlsCacheLimit(); err != nil {
					stopResourceGuard(err)
					return err
				}
				return nil
			},
		)
		return job.err
	}
	if directPlay {
		var direct ffmpeg.DirectWindowResult
		generateDirect := func() error {
			var err error
			direct, err = ffmpeg.GenerateDirectWindow(
				ctx,
				s.source.URLForJob(job.id),
				s.paths.outDir,
				info,
				selection,
				job.begin,
				segmentCount,
				m.settings.HlsSegmentDuration(),
				prerollBudget,
			)
			return err
		}
		var directErr error
		if ffmpeg.CopiesAudio(info, selection) {
			directErr = generateDirect()
		} else {
			directErr = m.withTranscodeSlot(ctx, generateDirect)
		}
		if directErr == nil {
			if err := m.checkHlsCacheLimit(); err != nil {
				stopResourceGuard(err)
				directErr = err
			}
		}
		if directErr == nil {
			job.fragments = direct.Fragments
			job.directEnd = direct.ReachedEnd
			job.result.ReachedEnd = direct.ReachedEnd
			s.markSegmentProgress(job, job.begin)
		} else if errors.Is(directErr, ffmpeg.ErrRemuxNeedsTranscode) {
			log.Printf("Direct HLS window fell back to transcoding: dir=%s segments=[%d,%d): %v", filepath.Base(s.paths.outDir), job.begin, job.end, directErr)
			release()
			selection.ForceTranscode = true
			nextRelease, reserveErr := m.reserveHlsGeneration(time.Duration(segmentCount)*m.settings.HlsSegmentDuration(), ffmpeg.HLSReservationBitrate(info, selection))
			if reserveErr != nil {
				job.err = reserveErr
			} else {
				release = nextRelease
				job.err = m.switchToTranscode(s, job)
			}
			if job.err == nil {
				job.err = m.withTranscodeSlot(ctx, generateTranscoded)
			}
		} else {
			job.err = directErr
		}
	} else {
		job.err = m.withTranscodeSlot(ctx, generateTranscoded)
	}
	if job.err == nil {
		s.mtx.Lock()
		stillDirect := s.directPlay
		s.mtx.Unlock()
		if stillDirect {
			job.err = m.publishDirectWindow(s, job)
		} else {
			job.err = m.publishTranscodedWindow(s, job)
		}
	}
	if job.err == nil && job.result.ReachedEnd && info.Duration > 0 {
		job.err = m.reconcileKnownDuration(s, job)
	}
	if job.err == nil {
		m.prefetchProgressiveSubtitles(s, job.begin, job.end)
	}
	if job.err != nil && !errors.Is(job.err, context.Canceled) {
		log.Printf("Generate HLS video window failed: dir=%s segments=[%d,%d): %v", filepath.Base(s.paths.outDir), job.begin, job.end, job.err)
	}
}

func (m *manager) directPrerollBudget(bitrate int64) time.Duration {
	windowDuration := time.Duration(m.settings.HlsWindowSegments()) * m.settings.HlsSegmentDuration()
	maximumDuration := windowGenerationTimeout
	if maximumDuration <= windowDuration {
		return 0
	}
	limit := m.settings.MaxCacheBytes()
	if limit <= 0 {
		return maximumDuration - windowDuration
	}
	budget := max(limit/int64(max(1, m.settings.MaxQueuedJobs())), estimatedHlsWindowBytes(windowDuration, bitrate))
	low := int64(math.Ceil(windowDuration.Seconds()))
	high := int64(maximumDuration / time.Second)
	for low < high {
		middle := low + (high-low+1)/2
		if estimatedHlsWindowBytes(time.Duration(middle)*time.Second, bitrate) <= budget {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return max(0, time.Duration(low)*time.Second-windowDuration)
}

func (m *manager) publishDirectWindow(s *streamInfo, job *segmentJob) error {
	s.mtx.Lock()
	fmp4 := ffmpeg.UsesFMP4(s.mediaInfo, s.selection)
	s.mtx.Unlock()
	return m.publishMaterializedWindow(s, job, job.fragments, fmp4, true, job.end, job.directEnd)
}

func (m *manager) publishTranscodedWindow(s *streamInfo, job *segmentJob) error {
	fragments := make([]ffmpeg.HLSFragment, 0, len(job.result.Durations))
	cursor := float64(job.begin) * m.settings.HlsSegmentDuration().Seconds()
	for offset, duration := range job.result.Durations {
		fragments = append(fragments, ffmpeg.HLSFragment{
			Start:    cursor,
			Duration: duration,
			Name:     videoSegmentName(job.begin + offset),
		})
		cursor += duration
	}
	return m.publishMaterializedWindow(
		s,
		job,
		fragments,
		false,
		false,
		job.begin+job.result.Generated,
		job.result.ReachedEnd,
	)
}

func (m *manager) publishMaterializedWindow(
	s *streamInfo,
	job *segmentJob,
	fragments []ffmpeg.HLSFragment,
	fmp4 bool,
	direct bool,
	advertisedEnd int,
	reachedEnd bool,
) error {
	s.playlistMtx.Lock()
	defer s.playlistMtx.Unlock()

	s.mtx.Lock()
	if s.directPlay != direct {
		s.mtx.Unlock()
		return context.Canceled
	}
	windows := make(map[int][]ffmpeg.HLSFragment, len(s.directWindows)+1)
	for owner, window := range s.directWindows {
		windows[owner] = window
	}
	window := append([]ffmpeg.HLSFragment(nil), fragments...)
	if direct {
		window = appendableDirectFragments(windows, window)
		if len(window) == 0 && !reachedEnd {
			s.mtx.Unlock()
			return fmt.Errorf("progressive remux produced no new GOP")
		}
	}
	if len(window) > 0 {
		window[0].Discontinuity = true
	}
	windows[job.begin] = window
	sequence := s.progressiveSequence
	discontinuitySequence := s.progressiveDiscontinuitySequence
	advertised := s.progressiveAdvertised
	ended := s.progressiveEnded
	assetVersion := s.assetVersion
	presentationTarget := s.presentationTarget
	info := s.mediaInfo
	selection := s.selection
	progressiveSubtitles := s.progressiveSubtitles
	progressiveLast := s.progressiveLast
	targetDuration := max(2*m.settings.HlsSegmentDuration(), maximumFragmentDuration(window))
	rotate := s.materializedTarget > 0 && targetDuration > s.materializedTarget
	if !rotate {
		targetDuration = max(targetDuration, s.materializedTarget)
	} else {
		assetVersion = fmt.Sprintf("%x", time.Now().UnixNano())
		windows = map[int][]ffmpeg.HLSFragment{job.begin: window}
		sequence = 0
		discontinuitySequence = 0
	}
	trimMaterializedTail(windows, &sequence, &discontinuitySequence, targetDuration)
	advertised = max(advertised, advertisedEnd)
	ended = ended || reachedEnd
	if s.mediaInfo.Duration > 0 {
		ended = ended || advertised >= segmentCount(s.mediaInfo.Duration, m.settings.HlsSegmentDuration())
	}
	allFragments := make([]ffmpeg.HLSFragment, 0)
	for _, window := range windows {
		allFragments = append(allFragments, window...)
	}
	s.mtx.Unlock()

	var err error
	if rotate {
		err = ffmpeg.PrepareOnDemandHLS(
			s.paths.outDir,
			s.paths.videoPlaylist,
			s.paths.subtitlePlaylist,
			s.paths.masterPlaylist,
			info,
			selection,
			m.settings.HlsSegmentDuration(),
			m.settings.HlsWindowSegments(),
			assetVersion,
		)
	}
	if err == nil {
		err = ffmpeg.UpdateMaterializedHLS(
			s.paths.videoPlaylist,
			targetDuration,
			assetVersion,
			fmp4,
			sequence,
			discontinuitySequence,
			presentationTarget,
			ended,
			allFragments,
		)
	}
	if err == nil && rotate && ffmpeg.UsesTextSubtitles(info, selection) {
		err = ffmpeg.UpdateProgressiveSubtitleHLS(
			s.paths.subtitlePlaylist,
			m.settings.HlsSegmentDuration(),
			m.settings.HlsWindowSegments(),
			assetVersion,
			progressiveSubtitles,
			progressiveLast,
			ended && progressiveSubtitles >= advertised,
		)
	}
	if err == nil {
		s.mtx.Lock()
		s.directWindows = windows
		s.assetVersion = assetVersion
		s.materializedTarget = targetDuration
		s.progressiveSequence = sequence
		s.progressiveDiscontinuitySequence = discontinuitySequence
		s.progressiveAdvertised = advertised
		s.progressiveEnded = ended
		s.mtx.Unlock()
	}
	return err
}

func maximumFragmentDuration(fragments []ffmpeg.HLSFragment) time.Duration {
	maximum := time.Duration(0)
	for _, fragment := range fragments {
		maximum = max(maximum, time.Duration(math.Ceil(fragment.Duration*float64(time.Second))))
	}
	return maximum
}

func trimMaterializedTail(windows map[int][]ffmpeg.HLSFragment, mediaSequence, discontinuitySequence *int, targetDuration time.Duration) {
	owners := make([]int, 0, len(windows))
	total := 0.0
	for owner, fragments := range windows {
		owners = append(owners, owner)
		for _, fragment := range fragments {
			total += fragment.Duration
		}
	}
	sort.Ints(owners)
	minimum := 3 * targetDuration.Seconds()
	for len(owners) > 1 {
		owner := owners[0]
		removedDuration := 0.0
		for _, fragment := range windows[owner] {
			removedDuration += fragment.Duration
		}
		if total-removedDuration < minimum-0.001 {
			break
		}
		*mediaSequence += len(windows[owner])
		(*discontinuitySequence)++
		delete(windows, owner)
		owners = owners[1:]
		total -= removedDuration
	}
}

func appendableDirectFragments(windows map[int][]ffmpeg.HLSFragment, incoming []ffmpeg.HLSFragment) []ffmpeg.HLSFragment {
	cursor := math.Inf(-1)
	for _, window := range windows {
		for _, fragment := range window {
			cursor = max(cursor, fragment.Start+fragment.Duration)
		}
	}
	sort.Slice(incoming, func(i, j int) bool {
		return incoming[i].Start < incoming[j].Start
	})
	result := incoming[:0]
	for _, fragment := range incoming {
		end := fragment.Start + fragment.Duration
		if end <= cursor+0.001 {
			continue
		}
		if !math.IsInf(cursor, -1) {
			if overlap := cursor - fragment.Start; overlap > 0.001 && overlap <= 0.25 {
				fragment.Start = cursor
			} else if fragment.Start < cursor-0.001 {
				continue
			}
		}
		result = append(result, fragment)
		cursor = fragment.Start + fragment.Duration
	}
	return result
}

func nextDirectWindowBegin(windows map[int][]ffmpeg.HLSFragment, segmentDuration time.Duration) int {
	if segmentDuration <= 0 {
		return 0
	}
	end := 0.0
	for _, window := range windows {
		for _, fragment := range window {
			end = max(end, fragment.Start+fragment.Duration)
		}
	}
	return max(0, int(math.Floor((end+0.001)/segmentDuration.Seconds())))
}

func (m *manager) switchToTranscode(s *streamInfo, current *segmentJob) error {
	s.mtx.Lock()
	if !s.directPlay {
		s.mtx.Unlock()
		return nil
	}
	s.directPlay = false
	s.directWindows = make(map[int][]ffmpeg.HLSFragment)
	s.materializedTarget = 0
	s.selection.ForceTranscode = true
	s.status.Mode = "transcode"
	for job := range s.videoJobs {
		if job != current && job.cancel != nil {
			job.cancel()
		}
	}
	info := s.mediaInfo
	selection := s.selection
	progressiveSubtitles := s.progressiveSubtitles
	progressiveLast := s.progressiveLast
	progressiveAdvertised := s.progressiveAdvertised
	progressiveEnded := s.progressiveEnded
	s.mtx.Unlock()

	s.playlistMtx.Lock()
	defer s.playlistMtx.Unlock()
	err := ffmpeg.PrepareOnDemandHLS(
		s.paths.outDir,
		s.paths.videoPlaylist,
		s.paths.subtitlePlaylist,
		s.paths.masterPlaylist,
		info,
		selection,
		m.settings.HlsSegmentDuration(),
		m.settings.HlsWindowSegments(),
		s.assetVersion,
	)
	if err == nil && ffmpeg.UsesTextSubtitles(info, selection) {
		err = ffmpeg.UpdateProgressiveSubtitleHLS(
			s.paths.subtitlePlaylist,
			m.settings.HlsSegmentDuration(),
			m.settings.HlsWindowSegments(),
			s.assetVersion,
			progressiveSubtitles,
			progressiveLast,
			progressiveEnded && progressiveSubtitles >= progressiveAdvertised,
		)
	}
	return err
}

func (m *manager) reconcileKnownDuration(s *streamInfo, job *segmentJob) error {
	actual := 0.0
	if len(job.fragments) > 0 {
		last := job.fragments[len(job.fragments)-1]
		actual = last.Start + last.Duration
	} else {
		actual = float64(job.begin) * m.settings.HlsSegmentDuration().Seconds()
		for _, duration := range job.result.Durations {
			actual += duration
		}
	}
	s.mtx.Lock()
	if actual >= s.mediaInfo.Duration-0.001 {
		s.mtx.Unlock()
		return nil
	}
	s.mediaInfo.Duration = actual
	s.mediaInfo.Seekable = actual > 0
	s.status.Duration = actual
	s.status.Seekable = actual > 0
	s.mtx.Unlock()
	return nil
}

func (m *manager) publishProgressiveSubtitle(s *streamInfo, index int) error {
	s.playlistMtx.Lock()
	defer s.playlistMtx.Unlock()

	s.mtx.Lock()
	if index < s.progressiveSubtitles {
		s.mtx.Unlock()
		return nil
	}
	if index != s.progressiveSubtitles {
		s.mtx.Unlock()
		return fmt.Errorf("progressive subtitle segment %d is not contiguous after %d", index, s.progressiveSubtitles)
	}
	count := s.progressiveSubtitles + 1
	last := s.progressiveLast
	ended := s.progressiveEnded && count >= s.progressiveAdvertised
	assetVersion := s.assetVersion
	s.mtx.Unlock()

	if err := ffmpeg.UpdateProgressiveSubtitleHLS(
		s.paths.subtitlePlaylist,
		m.settings.HlsSegmentDuration(),
		m.settings.HlsWindowSegments(),
		assetVersion,
		count,
		last,
		ended,
	); err != nil {
		return err
	}
	s.mtx.Lock()
	s.progressiveSubtitles = count
	s.mtx.Unlock()
	return nil
}

func (m *manager) GetHlsStatus(ctx context.Context, streamDir string, targetSeconds float64) (result domain.HlsStatus, resultErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = domain.HlsStatus{}
			resultErr = fmt.Errorf("torrent status panic: %v", recovered)
		}
	}()
	if err := ctx.Err(); err != nil {
		return domain.HlsStatus{}, err
	}
	key, err := parseStreamDir(streamDir)
	if err != nil {
		return domain.HlsStatus{}, fmt.Errorf("%w: bad stream: %v", domain.ErrBadHlsRequest, err)
	}
	m.mu.Lock()
	s, ok := m.active[key]
	m.mu.Unlock()
	if !ok {
		return domain.HlsStatus{}, domain.ErrHlsStreamNotFound
	}

	stats := s.torrent.Stats()
	usefulBytes := stats.BytesReadUsefulData.Int64()
	now := time.Now()
	targetIndex := -1
	if targetSeconds >= 0 && !math.IsNaN(targetSeconds) && !math.IsInf(targetSeconds, 0) {
		targetIndex = int(math.Floor(targetSeconds / m.settings.HlsSegmentDuration().Seconds()))
	}
	s.mtx.Lock()
	if usefulBytes > s.lastTorrentBytes {
		s.lastTorrentBytes = usefulBytes
	}
	status := s.status
	initialized := channelClosed(s.ready)
	videoJobActive := false
	subtitleJobActive := false
	withSubtitles := false
	directReady := false
	if targetIndex >= 0 && initialized {
		status.TargetSeconds = targetSeconds
		status.Seekable = s.mediaInfo.Seekable
		status.Duration = s.mediaInfo.Duration
		status.Message = ""
		withSubtitles = ffmpeg.UsesTextSubtitles(s.mediaInfo, s.selection)
		if s.directPlay {
			begin, _ := segmentWindow(targetIndex, segmentCount(s.mediaInfo.Duration, m.settings.HlsSegmentDuration()), m.settings.HlsWindowSegments())
			directReady = len(s.directWindows[begin]) > 0
		}
		if s.fatalErr != nil {
			status.Phase = "error"
			status.Message = publicStreamError(s.fatalErr)
		} else if failure, ok := s.segmentErrors[targetIndex]; ok {
			status.Phase = "error"
			status.Message = failure.message
			status.StartedAt = failure.at
			status.LastProgress = failure.at
		} else {
			videoJob := findSegmentJob(s.videoJobs, targetIndex)
			subtitleJob := findSegmentJob(s.subtitleJobs, targetIndex)
			videoJobActive = videoJob != nil
			subtitleJobActive = subtitleJob != nil
			job := videoJob
			if job == nil {
				job = subtitleJob
			}
			if job != nil {
				status.Phase = "preparing"
				status.StartedAt = job.startedAt
				status.LastProgress = job.lastProgress
				status.BytesRead = job.bytesRead
				if status.LastProgress.IsZero() {
					status.LastProgress = job.startedAt
				}
			} else {
				status.Phase = "waiting"
				status.StartedAt = now
				status.LastProgress = now
			}
		}
	}
	s.mtx.Unlock()
	if targetIndex >= 0 && initialized && status.Phase != "error" {
		videoReady := directReady
		if !videoReady {
			_, err := os.Stat(filepath.Join(s.paths.outDir, videoSegmentName(targetIndex)))
			videoReady = err == nil
		}
		subtitleReady := !withSubtitles
		if !subtitleReady {
			_, err := os.Stat(filepath.Join(s.paths.outDir, subtitleSegmentName(targetIndex)))
			subtitleReady = err == nil
		}
		if videoReady && subtitleReady {
			status.Phase = "ready"
			status.Message = ""
			status.LastProgress = now
		} else if videoJobActive || subtitleJobActive {
			status.Phase = "preparing"
		}
	}
	status.ActivePeers = stats.ActivePeers
	status.TotalPeers = stats.TotalPeers
	return classifyHlsStatus(status, now), nil
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func classifyHlsStatus(status domain.HlsStatus, now time.Time) domain.HlsStatus {
	if status.Phase == "probing" || status.Phase == "preparing" {
		idle := now.Sub(status.LastProgress)
		if status.ActivePeers == 0 && idle >= 5*time.Second {
			status.Phase = "no_peers"
			status.Message = "No active peers; discovery is still running"
		} else if idle >= 15*time.Second {
			status.Phase = "stalled"
			status.Message = "Connected peers or the media worker have not produced data recently"
		}
	}
	return status
}

func (m *manager) ensureSubtitleSegment(ctx context.Context, s *streamInfo, index int) error {
	s.mtx.Lock()
	closing := s.closing
	fatalErr := s.fatalErr
	info := s.mediaInfo
	selection := s.selection
	s.mtx.Unlock()
	if closing {
		return context.Canceled
	}
	if fatalErr != nil {
		return fatalErr
	}
	if !ffmpeg.UsesTextSubtitles(info, selection) {
		return errors.New("stream has no text subtitle rendition")
	}
	path := filepath.Join(s.paths.outDir, subtitleSegmentName(index))
	if touchExisting(path) {
		s.markPreparing(index, m.settings.HlsSegmentDuration())
		s.markReady(index)
		return nil
	}
	s.markPreparing(index, m.settings.HlsSegmentDuration())
	s.mtx.Lock()
	if s.closing {
		s.mtx.Unlock()
		return context.Canceled
	}
	if s.fatalErr != nil {
		err := s.fatalErr
		s.mtx.Unlock()
		return err
	}
	total := 0
	if s.mediaInfo.Duration > 0 {
		total = segmentCount(s.mediaInfo.Duration, m.settings.HlsSegmentDuration())
		if index < 0 || index >= total {
			s.mtx.Unlock()
			return errors.New("subtitle segment out of range")
		}
	} else {
		total = s.progressiveAdvertised
		if index < 0 || index >= total {
			s.mtx.Unlock()
			return errors.New("subtitle segment is outside the discovered range")
		}
	}
	job := findSegmentJob(s.subtitleJobs, index)
	if job == nil {
		begin, end := segmentWindow(index, total, m.settings.HlsWindowSegments())
		cancelAbandonedJobsLocked(s.subtitleJobs, begin, end)
		if err := m.reserveJobSlotLocked(s); err != nil {
			s.mtx.Unlock()
			s.markSegmentError(index, err)
			return fmt.Errorf("%w: %v", domain.ErrHlsTemporarilyUnavailable, err)
		}
		jobCtx, cancel := context.WithCancel(s.ctx)
		now := time.Now()
		job = &segmentJob{begin: begin, end: end, id: segmentJobID("subtitle", begin, end, now), cancel: cancel, done: make(chan struct{}), startedAt: now, lastProgress: now, slotHeld: true}
		if s.subtitleJobs == nil {
			s.subtitleJobs = make(map[*segmentJob]struct{})
		}
		s.subtitleJobs[job] = struct{}{}
		go m.runSubtitleJob(s, jobCtx, job)
	}
	job.waiters++
	s.mtx.Unlock()
	err := waitForGeneratedAsset(ctx, path, job)
	m.releaseJobWaiter(s, job, err)
	if err == nil {
		s.markReady(index)
	}
	return err
}

func (m *manager) runSubtitleJob(s *streamInfo, ctx context.Context, job *segmentJob) {
	defer func() {
		if recovered := recover(); recovered != nil {
			job.err = fmt.Errorf("subtitle generation panic: %v", recovered)
		}
		if errors.Is(job.err, context.Canceled) {
			s.markJobCanceled(job)
		} else {
			s.markJobError(job, job.err)
		}
		close(job.done)
		s.mtx.Lock()
		delete(s.subtitleJobs, job)
		s.mtx.Unlock()
		m.releaseJobSlot(job)
		m.enforceCacheLimit()
	}()
	ctx, cancel := context.WithTimeout(ctx, windowGenerationTimeout)
	defer cancel()
	s.mtx.Lock()
	info := s.mediaInfo
	s.mtx.Unlock()
	duration := time.Duration(job.end-job.begin) * m.settings.HlsSegmentDuration()
	release, err := m.reserveHlsGeneration(duration, 0)
	if err != nil {
		job.err = err
		return
	}
	defer release()
	ctx, stopResourceGuard := context.WithCancelCause(ctx)
	resourceGuard := m.monitorHlsResources(ctx, stopResourceGuard)
	defer func() {
		stopResourceGuard(context.Canceled)
		resourceErr := <-resourceGuard
		if cause := context.Cause(ctx); resourceErr == nil && cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
			resourceErr = cause
		}
		if resourceErr != nil && (job.err == nil || errors.Is(job.err, context.Canceled)) {
			job.err = resourceErr
		}
	}()
	job.err = m.withTranscodeSlot(ctx, func() error {
		s.markJobStarted(job)
		for index := job.begin; index < job.end; index++ {
			path := filepath.Join(s.paths.outDir, subtitleSegmentName(index))
			if touchExisting(path) {
				continue
			}
			if err := ffmpeg.GenerateSubtitleSegment(
				ctx,
				s.source.URLForJob(job.id),
				path,
				s.selection.SubtitleTrackIndex,
				index,
				m.settings.HlsSegmentDuration(),
			); err != nil {
				return err
			}
			if err := m.checkHlsCacheLimit(); err != nil {
				stopResourceGuard(err)
				return err
			}
			s.markSegmentProgress(job, index)
			if info.Duration <= 0 {
				if err := m.publishProgressiveSubtitle(s, index); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if job.err != nil && !errors.Is(job.err, context.Canceled) {
		log.Printf("Generate HLS subtitles failed: dir=%s segments=[%d,%d): %v", filepath.Base(s.paths.outDir), job.begin, job.end, job.err)
	}
}

func findSegmentJob(jobs map[*segmentJob]struct{}, index int) *segmentJob {
	for job := range jobs {
		if index >= job.begin && index < job.end && !jobFinished(job) {
			return job
		}
	}
	return nil
}

func segmentWindow(index, total, window int) (begin, end int) {
	if window <= 0 {
		window = 1
	}
	begin = index / window * window
	end = begin + window
	if total > 0 {
		end = min(end, total)
	}
	return begin, end
}

func segmentJobID(kind string, begin, end int, started time.Time) string {
	return fmt.Sprintf("%s-%d-%d-%d", kind, begin, end, started.UnixNano())
}

func cancelAbandonedJobsLocked(jobs map[*segmentJob]struct{}, begin, end int) {
	for job := range jobs {
		overlaps := job.begin < end && begin < job.end
		if overlaps || job.background || job.waiters > 0 || jobFinished(job) || job.cancel == nil {
			continue
		}
		job.cancel()
	}
}

// reserveJobSlotLocked applies both per-stream fairness and a hard global cap.
// The caller must hold s.mtx until the new job is inserted into its map.
func (m *manager) reserveJobSlotLocked(s *streamInfo) error {
	if len(s.videoJobs)+len(s.subtitleJobs) >= m.settings.MaxJobsPerStream() {
		return errStreamJobLimit
	}
	if m.jobs == nil {
		return nil
	}
	select {
	case m.jobs <- struct{}{}:
		return nil
	default:
		return errStreamJobQueueFull
	}
}

func (m *manager) releaseJobSlot(job *segmentJob) {
	if job == nil || !job.slotHeld || m.jobs == nil {
		return
	}
	job.slotHeld = false
	<-m.jobs
}

func (m *manager) releaseJobWaiter(s *streamInfo, job *segmentJob, waitErr error) {
	s.mtx.Lock()
	if job.waiters > 0 {
		job.waiters--
	}
	// Once FFmpeg has started, keep producing the bounded canonical window even
	// if a browser or reverse proxy closes its HTTP request. A retry can then
	// join the same job instead of throwing away minutes of torrent/CPU work.
	if waitErr != nil && job.waiters == 0 && !job.background && !job.started && !jobFinished(job) {
		job.cancel()
	}
	s.mtx.Unlock()
}

func (s *streamInfo) markJobStarted(job *segmentJob) {
	s.mtx.Lock()
	job.started = true
	job.lastProgress = time.Now()
	s.mtx.Unlock()
}

func (m *manager) withTranscodeSlot(ctx context.Context, run func() error) error {
	if m.transcodes == nil {
		return run()
	}
	select {
	case m.transcodes <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-m.transcodes
			return err
		}
		defer func() { <-m.transcodes }()
		return run()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitForGeneratedAsset(ctx context.Context, path string, job *segmentJob) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if touchExisting(path) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-job.done:
			if touchExisting(path) {
				return nil
			}
			if job.err != nil {
				return job.err
			}
			return errors.New("HLS asset was not generated")
		case <-ticker.C:
		}
	}
}

func waitForJob(ctx context.Context, job *segmentJob) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-job.done:
		return job.err
	}
}

func jobFinished(job *segmentJob) bool {
	select {
	case <-job.done:
		return true
	default:
		return false
	}
}

func touchExisting(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if time.Since(info.ModTime()) >= hlsTouchInterval {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
	}
	return true
}

func segmentCount(duration float64, segmentDuration time.Duration) int {
	return int(math.Ceil(duration / segmentDuration.Seconds()))
}

func videoSegmentName(index int) string {
	return "chunk_" + formatSegmentIndex(index) + ".ts"
}

func subtitleSegmentName(index int) string {
	return "subs_" + formatSegmentIndex(index) + ".vtt"
}

func formatSegmentIndex(index int) string {
	return fmt.Sprintf("%06d", index)
}

func parseSegmentName(name, prefix, suffix string) (int, bool) {
	if filepath.Base(name) != name || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	if len(raw) != 6 {
		return 0, false
	}
	index, err := strconv.Atoi(raw)
	return index, err == nil && index >= 0
}

func parseDirectSegmentOwner(name string) (int, bool) {
	if filepath.Base(name) != name || !strings.HasPrefix(name, "direct_") {
		return 0, false
	}
	extension := filepath.Ext(name)
	if extension != ".ts" && extension != ".m4s" {
		return 0, false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(name, "direct_"), extension), "_")
	if len(parts) != 2 || len(parts[0]) != 6 || len(parts[1]) != 4 {
		return 0, false
	}
	owner, ownerErr := strconv.Atoi(parts[0])
	position, positionErr := strconv.Atoi(parts[1])
	return owner, ownerErr == nil && positionErr == nil && owner >= 0 && position >= 0
}

func parseDirectInitOwner(name string) (int, bool) {
	return parseSegmentName(name, "init_", ".mp4")
}
