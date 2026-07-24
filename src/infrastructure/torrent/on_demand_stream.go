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
	errSourceBlocked      = errors.New("packager is blocked on torrent source data")
	errPackagerNoOutput   = errors.New("packager made no output progress")
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
			s.status.Phase = domain.HlsPhaseError
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
		m.storeMediaDescriptor(s.ctx, mediaKey{InfoHash: key.InfoHash, Index: key.Index}, info)
	}
	if err == nil {
		info.Seekable = info.Duration > 0
	}
	target := m.timeline(info.Duration).locate(s.presentationTarget)
	directPlay := err == nil && ffmpeg.CanRemuxHLS(info, s.selection)
	if err == nil {
		err = func() error {
			s.playlistMtx.Lock()
			defer s.playlistMtx.Unlock()
			return m.media.publishHls(
				estimatedHlsMetadataBytes(info.Duration, m.settings.HlsSegmentDuration(), s.selection.SubtitleTrackIndex >= 0),
				8,
				func() error {
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
				},
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
		s.status.Phase = domain.HlsPhaseError
		if s.status.Message == "" {
			s.status.Message = publicStreamError(err)
		}
	} else {
		s.presentationTarget = target.sourceSeconds
		s.materializedEnd = target.segment
		if ffmpeg.UsesTextSubtitles(info, s.selection) && info.Duration > 0 {
			s.progressiveSubtitles = m.timeline(info.Duration).segmentCount()
			s.progressiveLast = info.Duration - m.timeline(0).segmentStart(s.progressiveSubtitles-1)
		} else {
			s.progressiveSubtitles = target.segment
		}
		s.status.Phase = domain.HlsPhaseWaiting
		s.status.Mode = ffmpeg.HLSMode(info, s.selection)
		s.status.TargetSeconds = target.sourceSeconds
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

func (m *manager) timeline(durationSeconds float64) playbackTimeline {
	return newPlaybackTimeline(m.settings.HlsSegmentDuration(), m.settings.HlsWindowSegments(), durationSeconds)
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
		currentVersion := stream.currentAssetVersion()
		if (assetName != "master.m3u8" || version != "") && version != currentVersion {
			if unlockPlaylist != nil {
				unlockPlaylist()
			}
			stream.generationMtx.RUnlock()
			return application.HlsAsset{}, domain.ErrHlsPlaylistChanged
		}
		asset, err := m.media.openHls(filepath.Join(m.settings.HlsPath(), streamDir, assetName))
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
	staleVersion := (assetName != "master.m3u8" || version != "") && version != currentVersion
	if strings.HasSuffix(assetName, ".m3u8") {
		if staleVersion {
			return domain.ErrHlsPlaylistChanged
		}
		if assetName == filepath.Base(s.paths.videoPlaylist) {
			s.mtx.Lock()
			needsStartup := s.playlistTargetDuration <= 0
			s.mtx.Unlock()
			if needsStartup {
				m.prefetchProgressiveWindow(s, -1)
			}
		}
		return nil
	}
	owner, directAsset := parseDirectSegmentOwner(assetName)
	if !directAsset {
		owner, directAsset = parseDirectInitOwner(assetName)
	}
	if directAsset {
		path := filepath.Join(s.paths.outDir, assetName)
		if staleVersion {
			if m.media.touchHls(path) {
				return nil
			}
			return domain.ErrHlsPlaylistChanged
		}
		if m.media.touchHls(path) {
			s.mtx.Lock()
			requested := directPrefetchIndexLocked(s, m.timeline(s.mediaInfo.Duration), owner, assetName)
			s.mtx.Unlock()
			m.prefetchProgressiveWindow(s, requested)
			return nil
		}
		s.mtx.Lock()
		delete(s.materializedWindows, owner)
		delete(s.materializedBytes, owner)
		s.mtx.Unlock()
		if err := m.ensureVideoSegment(ctx, s, owner); err != nil {
			return err
		}
		if !m.media.touchHls(path) {
			return domain.ErrHlsPlaylistChanged
		}
		s.mtx.Lock()
		requested := directPrefetchIndexLocked(s, m.timeline(s.mediaInfo.Duration), owner, assetName)
		s.mtx.Unlock()
		m.prefetchProgressiveWindow(s, requested)
		return nil
	}
	if staleVersion {
		return domain.ErrHlsPlaylistChanged
	}
	if index, ok := parseSegmentName(assetName, "chunk_", ".ts"); ok {
		path := filepath.Join(s.paths.outDir, assetName)
		begin, _ := m.timeline(0).windowForSegment(index)
		if m.media.touchHls(path) {
			m.prefetchProgressiveWindow(s, index)
			return nil
		}
		s.mtx.Lock()
		delete(s.materializedWindows, begin)
		delete(s.materializedBytes, begin)
		s.mtx.Unlock()
		if err := m.ensureVideoSegment(ctx, s, index); err != nil {
			return err
		}
		if !m.media.touchHls(path) {
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

// requestVideoTarget turns a prepare request into work owned by the existing
// playback session. It never creates a second presentation for a seek.
func (m *manager) requestVideoTarget(s *streamInfo, target playbackTarget) {
	if s == nil {
		return
	}
	if !channelClosed(s.ready) {
		go func() {
			if s.waitReady(s.ctx) == nil {
				m.requestVideoTarget(s, target)
			}
		}()
		return
	}

	s.markPreparing(target.segment, target.sourceSeconds)
	s.playlistMtx.RLock()
	s.mtx.Lock()
	if s.closing || s.fatalErr != nil {
		s.mtx.Unlock()
		s.playlistMtx.RUnlock()
		return
	}
	current := fragmentsCoverTime(s.publishedFragments, target.sourceSeconds)
	cached := !current && directFragmentsCoverTime(s.materializedWindows, target.sourceSeconds)
	if current || cached {
		if current {
			retargetVideoJobsLocked(s.videoJobs, target.sourceSeconds)
		} else {
			claimVideoTargetLocked(s.videoJobs, target.segment, target.segment+1, target.sourceSeconds)
		}
		setPlaybackTargetLocked(s, target)
		s.mtx.Unlock()
		s.playlistMtx.RUnlock()
		if cached {
			if err := m.publishCachedTarget(s, target); err != nil {
				s.markSegmentError(target.segment, err)
				return
			}
		}
		s.markReady(target.segment)
		m.requestSelectedSubtitleTarget(s, target)
		m.prefetchProgressiveWindow(s, target.segment)
		return
	}
	timeline := m.timeline(s.mediaInfo.Duration)
	if !timeline.containsSegment(target.segment) {
		s.mtx.Unlock()
		s.playlistMtx.RUnlock()
		s.markSegmentError(target.segment, errors.New("video segment out of range"))
		return
	}
	begin := target.segment
	end := begin + 1
	if total := timeline.segmentCount(); total > 0 {
		end = min(end, total)
	}
	// A prepare request is an explicit viewer target. Requests still observing
	// the previous presentation are obsolete after this point and must release
	// their slots before the new target is admitted.
	claimVideoTargetLocked(s.videoJobs, begin, end, target.sourceSeconds)
	job, jobCtx, created, err := s.acquireJobLocked(videoSegmentJob, target.segment, begin, end, false, m.scheduler, m.settings.MaxJobsPerStream())
	if err != nil {
		s.mtx.Unlock()
		s.playlistMtx.RUnlock()
		s.markSegmentError(target.segment, err)
		return
	}
	setPlaybackTargetLocked(s, target)
	job.targetSeconds = target.sourceSeconds
	s.mtx.Unlock()
	s.playlistMtx.RUnlock()
	if created {
		go m.runVideoJob(s, jobCtx, job)
	}
}

func setPlaybackTargetLocked(s *streamInfo, target playbackTarget) {
	s.presentationTarget = target.sourceSeconds
	s.playheadSegment = target.segment
}

func validateSegmentIndex(timeline playbackTimeline, discovered, index int, media string) error {
	if total := timeline.segmentCount(); total > 0 {
		if !timeline.containsSegment(index) {
			return fmt.Errorf("%s segment out of range", media)
		}
		return nil
	}
	if index < 0 || index >= discovered {
		return fmt.Errorf("%s segment is outside the discovered range", media)
	}
	return nil
}

func (m *manager) ensureVideoSegment(ctx context.Context, s *streamInfo, index int) error {
	s.mtx.Lock()
	streamErr := s.availabilityErrorLocked()
	directPlay := s.directPlay
	s.mtx.Unlock()
	if streamErr != nil {
		return streamErr
	}
	path := filepath.Join(s.paths.outDir, videoSegmentName(index))
	if m.media.touchHls(path) {
		s.markPreparing(index, m.timeline(0).segmentStart(index))
		s.markReady(index)
		m.prefetchProgressiveWindow(s, index)
		return nil
	}

	s.mtx.Lock()
	if err := s.availabilityErrorLocked(); err != nil {
		s.mtx.Unlock()
		return err
	}
	timeline := m.timeline(s.mediaInfo.Duration)
	if err := validateSegmentIndex(timeline, s.materializedEnd, index, "video"); err != nil {
		s.mtx.Unlock()
		return err
	}
	s.mtx.Unlock()

	s.markPreparing(index, m.timeline(0).segmentStart(index))
	s.mtx.Lock()
	if err := s.availabilityErrorLocked(); err != nil {
		s.mtx.Unlock()
		return err
	}
	if directPlay {
		begin, _ := timeline.windowForSegment(index)
		if len(s.materializedWindows[begin]) > 0 {
			s.mtx.Unlock()
			s.markReady(index)
			return nil
		}
	}
	begin, end := timeline.windowForSegment(index)
	job, jobCtx, created, err := s.acquireJobLocked(videoSegmentJob, index, begin, end, false, m.scheduler, m.settings.MaxJobsPerStream())
	if err != nil {
		s.mtx.Unlock()
		s.markSegmentError(index, err)
		return fmt.Errorf("%w: %v", domain.ErrHlsTemporarilyUnavailable, err)
	}
	if created {
		job.targetSeconds = s.presentationTarget
		go m.runVideoJob(s, jobCtx, job)
	}
	job.waiters++
	s.mtx.Unlock()
	var waitErr error
	if directPlay {
		waitErr = waitForDirectTarget(ctx, s, job)
	} else {
		waitErr = waitForGeneratedAsset(ctx, m.media, path, job)
	}
	s.releaseJobWaiter(job, waitErr)
	if waitErr == nil {
		s.markReady(index)
		// An incrementally published direct fragment can satisfy playback before
		// the admitted window finishes. The running job still owns continuation:
		// advancing the playhead here would make its remaining publications target
		// a later segment than the prefix is expected to cover.
		if jobFinished(job) {
			m.prefetchProgressiveWindow(s, index)
		}
	}
	return waitErr
}

// prefetchProgressiveWindow restores the two byte-bounded halves around the
// playhead. The adaptive time reserve controls forward urgency only: once it
// is healthy, preemptible work fills the backward and remaining forward disk
// horizons.
func (m *manager) prefetchProgressiveWindow(s *streamInfo, requested int) {
	maximumJob := m.settings.HlsWindowSegments()
	sideBytes := m.playbackCacheSideBytes()
	s.mtx.Lock()
	target := m.timeline(s.mediaInfo.Duration).locate(s.presentationTarget)
	requiresTargetSubtitles := !s.closing && ffmpeg.UsesTextSubtitles(s.mediaInfo, s.selection) &&
		directFragmentsCoverTime(s.materializedWindows, target.sourceSeconds)
	s.mtx.Unlock()
	waitingForTargetSubtitle := requiresTargetSubtitles &&
		!m.media.touchHls(filepath.Join(s.paths.outDir, subtitleSegmentName(target.segment)))
	if waitingForTargetSubtitle {
		m.requestSelectedSubtitleTarget(s, target)
	}
	if requested >= 0 {
		s.mtx.Lock()
		s.playheadSegment = max(s.playheadSegment, requested)
		shouldSlide := s.playlistTargetDuration > 0 && requested > s.playlistAnchor
		s.mtx.Unlock()
		if shouldSlide {
			if err := m.publishCachedDemand(s, requested); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("slide HLS presentation failed: dir=%s demand=%d: %v", filepath.Base(s.paths.outDir), requested, err)
			}
		}
	}
	if waitingForTargetSubtitle {
		// The selected subtitle is part of startup readiness. Do not admit a
		// later media packager while its target segment is still pending; the
		// subtitle job resumes progressive filling after it publishes.
		return
	}
	s.mtx.Lock()
	if s.closing || s.progressiveRetry {
		s.mtx.Unlock()
		return
	}
	for job := range s.videoJobs {
		if !jobFinished(job) {
			s.mtx.Unlock()
			return
		}
	}
	timeline := m.timeline(s.mediaInfo.Duration)
	demand := max(0, s.playheadSegment)
	total := 0
	if s.mediaInfo.Duration > 0 {
		total = timeline.segmentCount()
	} else if s.sourceEnded {
		total = s.materializedEnd
	}
	cacheWindow := playbackCacheWindow{
		sideBytes:       sideBytes,
		bytesPerSegment: estimatedPlaybackSegmentBytes(s.mediaInfo, s.selection, m.settings.HlsSegmentDuration()),
		maximumJob:      maximumJob,
		segmentDuration: m.settings.HlsSegmentDuration(),
		urgentReserve:   s.urgentAhead,
	}
	plan := cacheWindow.plan(s.materializedWindows, s.materializedBytes, timeline, demand, total, requested < 0 && len(s.materializedWindows) == 0)
	if plan.end <= plan.begin {
		s.mtx.Unlock()
		return
	}
	begin := plan.begin
	if s.directPlay && len(s.materializedWindows) > 0 && plan.begin >= demand && plan.begin > 0 {
		// Direct copy needs the GOP crossing the previous nominal boundary.
		begin--
	}
	job, jobCtx, created, err := s.acquireJobLocked(videoSegmentJob, plan.begin, begin, plan.end, plan.background, m.scheduler, m.settings.MaxJobsPerStream())
	if err != nil {
		s.progressiveRetry = true
		s.status.Phase = domain.HlsPhaseWaiting
		s.status.Message = "The server transcode queue is busy; retrying"
		s.status.LastProgress = time.Now()
		s.mtx.Unlock()
		go m.retryProgressivePrefetch(s)
		return
	}
	if !created {
		s.mtx.Unlock()
		return
	}
	job.targetSeconds = s.presentationTarget
	s.mtx.Unlock()
	if requested < 0 {
		s.markPreparing(begin, m.timeline(0).segmentStart(begin))
	}
	go m.runVideoJob(s, jobCtx, job)
}

func directPrefetchIndexLocked(s *streamInfo, timeline playbackTimeline, owner int, assetName string) int {
	fragments := s.materializedWindows[owner]
	if len(fragments) > 0 && fragments[len(fragments)-1].Name == assetName {
		last := fragments[len(fragments)-1]
		if last.Duration > 0 {
			return max(owner, timeline.locate(last.Start+last.Duration-0.001).segment)
		}
	}
	return owner
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

func (m *manager) runVideoJob(s *streamInfo, ctx context.Context, job *segmentJob) {
	defer func() {
		if recovered := recover(); recovered != nil {
			job.err = fmt.Errorf("video generation panic: %v", recovered)
		}
		s.completeJob(videoSegmentJob, job)
		m.enforceCacheLimit()
		if job.err == nil {
			s.recordJobDelivery(job, m.settings.HlsSegmentDuration(), time.Now())
			s.mtx.Lock()
			demand := s.playheadSegment
			target := m.timeline(s.mediaInfo.Duration).locate(s.presentationTarget)
			s.mtx.Unlock()
			m.requestSelectedSubtitleTarget(s, target)
			m.prefetchProgressiveWindow(s, demand)
			m.maybeRefineStreamDuration(s)
		}
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
	ctx, stopResourceGuard, finishResourceGuard := m.guardHlsJobResources(ctx, job)
	defer finishResourceGuard()
	s.markJobStarted(job)

	transcodedFragments := make([]ffmpeg.HLSFragment, 0, segmentCount)
	generateTranscoded := func(runCtx context.Context) error {
		s.mtx.Lock()
		existing := append([]ffmpeg.HLSFragment(nil), s.materializedWindows[job.materializationBegin]...)
		s.mtx.Unlock()
		var materializationBegin int
		transcodedFragments, materializationBegin = contiguousTranscodedFragments(existing, job.materializationBegin, job.end)
		if materializationBegin >= job.end {
			job.fragments = append(job.fragments[:0], transcodedFragments...)
			job.result = transcodedWindowResult(transcodedFragments, false)
			return nil
		}
		cursor := m.timeline(0).segmentStart(job.materializationBegin)
		if count := len(transcodedFragments); count > 0 {
			last := transcodedFragments[count-1]
			cursor = last.Start + last.Duration
		}
		result, err := ffmpeg.GenerateVideoWindow(
			runCtx,
			s.source.URLForJob(job.id),
			s.paths.outDir,
			info,
			selection,
			materializationBegin,
			job.end-materializationBegin,
			m.settings.HlsSegmentDuration(),
			func(index int, duration float64) error {
				fragment := ffmpeg.HLSFragment{
					Start:    cursor,
					Duration: duration,
					Name:     videoSegmentName(index),
				}
				transcodedFragments = append(transcodedFragments, fragment)
				cursor += duration
				s.markJobStage(job, domain.HlsStagePublishing)
				if err := m.publishMaterializedWindow(s, job, transcodedFragments, false, false, index+1, false); err != nil {
					return err
				}
				if stat, err := os.Stat(filepath.Join(s.paths.outDir, fragment.Name)); err == nil {
					s.markPublished(job, fragment.Name, stat.Size())
				}
				s.markSegmentProgress(job, index)
				s.markJobStage(job, domain.HlsStagePackaging)
				if err := m.checkHlsCacheLimit(); err != nil {
					stopResourceGuard(err)
					return err
				}
				return nil
			},
		)
		job.fragments = append(job.fragments[:0], transcodedFragments...)
		job.result = transcodedWindowResult(transcodedFragments, err == nil && result.ReachedEnd)
		return err
	}
	if directPlay {
		var direct ffmpeg.DirectWindowResult
		pending := make([]ffmpeg.HLSFragment, 0, 1)
		published := false
		publishFragment := func(fragment ffmpeg.HLSFragment) error {
			pending = append(pending, fragment)
			if !published {
				s.mtx.Lock()
				covered := directFragmentsCoverTime(s.materializedWindows, job.targetSeconds)
				s.mtx.Unlock()
				if !covered && !fragmentsCoverTime(pending, job.targetSeconds) {
					return nil
				}
			}
			fragmentEnd := fragment.Start + fragment.Duration - 0.001
			advertisedEnd := m.timeline(info.Duration).locate(fragmentEnd).segment + 1
			s.markJobStage(job, domain.HlsStagePublishing)
			if err := m.publishMaterializedWindow(s, job, pending, ffmpeg.UsesFMP4(info, selection), true, advertisedEnd, false); err != nil {
				return err
			}
			if stat, err := os.Stat(filepath.Join(s.paths.outDir, fragment.Name)); err == nil {
				s.markPublished(job, fragment.Name, stat.Size())
			}
			pending = pending[:0]
			published = true
			s.markSegmentProgress(job, advertisedEnd-1)
			s.markJobStage(job, domain.HlsStagePackaging)
			return m.checkHlsCacheLimit()
		}
		generateDirect := func(runCtx context.Context) error {
			var err error
			direct, err = ffmpeg.GenerateDirectWindow(
				runCtx,
				s.source.URLForJob(job.id),
				s.paths.outDir,
				info,
				selection,
				job.begin,
				job.materializationBegin,
				segmentCount,
				m.settings.HlsSegmentDuration(),
				prerollBudget,
				publishFragment,
			)
			return err
		}
		directWorker := mediaEncoderWorker
		if ffmpeg.CopiesAudio(info, selection) {
			directWorker = mediaPackagerWorker
		}
		directErr := m.runBoundedPackager(ctx, s, job, directWorker, generateDirect)
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
			s.markSegmentProgress(job, job.materializationBegin)
		} else if err := ctx.Err(); err != nil {
			// A superseded direct job must never change the representation mode.
			// Cancellation can arrive while the remuxer is classifying its final
			// probe, so the job context is authoritative over the returned error.
			job.err = err
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
				job.err = m.runBoundedPackager(ctx, s, job, mediaEncoderWorker, generateTranscoded)
			}
		} else {
			job.err = directErr
		}
	} else {
		job.err = m.runBoundedPackager(ctx, s, job, mediaEncoderWorker, generateTranscoded)
	}
	if job.err == nil {
		s.mtx.Lock()
		stillDirect := s.directPlay
		s.mtx.Unlock()
		if stillDirect {
			job.err = m.publishDirectWindow(s, job)
		} else {
			job.err = m.publishMaterializedWindow(
				s,
				job,
				job.fragments,
				false,
				false,
				job.materializationBegin+job.result.Generated,
				job.result.ReachedEnd,
			)
		}
	}
	if job.err == nil && job.result.ReachedEnd && info.Duration > 0 {
		job.err = m.reconcileKnownDuration(s, job)
	}
	if job.err != nil && !errors.Is(job.err, context.Canceled) {
		log.Printf("Generate HLS video window failed: dir=%s segments=[%d,%d): %v", filepath.Base(s.paths.outDir), job.begin, job.end, job.err)
	}
}

func (m *manager) maybeRefineStreamDuration(s *streamInfo) {
	targetAhead := max(1, int(math.Ceil(30/max(1.0, m.settings.HlsSegmentDuration().Seconds()))))
	s.mtx.Lock()
	if s.closing || s.durationRefinementStarted || s.materializedEnd-s.playheadSegment < targetAhead {
		s.mtx.Unlock()
		return
	}
	s.durationRefinementStarted = true
	info := s.mediaInfo
	key := s.mediaKey
	s.mtx.Unlock()

	go func() {
		refined, err := s.source.refineDurationFromTail(s.ctx, info)
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				log.Printf("background duration refinement failed: hash=%s file=%d: %v", key.InfoHash, key.Index, err)
			}
			return
		}
		if math.Abs(refined.Duration-info.Duration) <= 0.25 {
			return
		}
		s.mtx.Lock()
		if s.closing {
			s.mtx.Unlock()
			return
		}
		s.mediaInfo.Duration = refined.Duration
		s.mediaInfo.Seekable = refined.Seekable
		s.status.Duration = refined.Duration
		s.status.Seekable = refined.Seekable
		s.mtx.Unlock()
		m.storeMediaDescriptor(s.ctx, key, refined)
	}()
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

func (m *manager) guardHlsJobResources(ctx context.Context, job *segmentJob) (context.Context, context.CancelCauseFunc, func()) {
	ctx, stop := context.WithCancelCause(ctx)
	done := m.monitorHlsResources(ctx, stop)
	return ctx, stop, func() {
		stop(context.Canceled)
		resourceErr := <-done
		cause := context.Cause(ctx)
		if resourceErr == nil && cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
			resourceErr = cause
		}
		if resourceErr != nil && (job.err == nil || errors.Is(job.err, context.Canceled)) {
			job.err = resourceErr
		}
	}
}

func (m *manager) publishDirectWindow(s *streamInfo, job *segmentJob) error {
	s.mtx.Lock()
	fmp4 := ffmpeg.UsesFMP4(s.mediaInfo, s.selection)
	s.mtx.Unlock()
	return m.publishMaterializedWindow(s, job, job.fragments, fmp4, true, job.end, job.directEnd)
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
	sideBytes := m.playbackCacheSideBytes()
	materializationBegin := job.materializationBegin
	s.playlistMtx.Lock()
	defer s.playlistMtx.Unlock()

	s.mtx.Lock()
	if _, active := s.videoJobs[job]; !active || (job.ctx != nil && job.ctx.Err() != nil) {
		s.mtx.Unlock()
		return context.Canceled
	}
	if s.directPlay != direct {
		s.mtx.Unlock()
		return context.Canceled
	}
	windows := make(map[int][]ffmpeg.HLSFragment, len(s.materializedWindows)+1)
	for owner, window := range s.materializedWindows {
		windows[owner] = window
	}
	window := append([]ffmpeg.HLSFragment(nil), fragments...)
	if direct {
		existing := append([]ffmpeg.HLSFragment(nil), windows[materializationBegin]...)
		delete(windows, materializationBegin)
		window = append(existing, window...)
		window = appendableDirectFragments(windows, window)
		if len(window) == 0 && !reachedEnd {
			target := m.timeline(s.mediaInfo.Duration).segmentStart(job.begin) + 0.001
			if directFragmentsCoverTime(windows, target) {
				s.mtx.Unlock()
				return nil
			}
			s.mtx.Unlock()
			return fmt.Errorf("progressive remux produced no new GOP")
		}
	}
	if len(window) > 0 {
		window[0].Discontinuity = true
	}
	windows[materializationBegin] = window
	sourceBitrate := s.mediaInfo.Bitrate
	if sourceBitrate <= 0 {
		sourceBitrate = 8_000_000
	}
	knownCosts := make(map[int]int64, len(s.materializedBytes)+1)
	for owner, bytes := range s.materializedBytes {
		knownCosts[owner] = bytes
	}
	knownCosts[materializationBegin] = materializedWindowBytes(
		s.paths.outDir,
		window,
		sourceBitrate,
		ffmpeg.HLSReservationBitrate(s.mediaInfo, s.selection),
	)
	costs := materializedWindowCosts(
		s.paths.outDir,
		windows,
		knownCosts,
		sourceBitrate,
		ffmpeg.HLSReservationBitrate(s.mediaInfo, s.selection),
	)
	sequence := s.playlistSequence
	discontinuitySequence := s.playlistDiscontinuitySequence
	advertised := s.materializedEnd
	sourceEnded := s.sourceEnded
	assetVersion := s.assetVersion
	presentationTarget := job.targetSeconds
	info := s.mediaInfo
	selection := s.selection
	progressiveSubtitles := s.progressiveSubtitles
	progressiveLast := s.progressiveLast
	previousFragments := append([]ffmpeg.HLSFragment(nil), s.publishedFragments...)
	previousAnchor := s.playlistAnchor
	retainedAssets := make(map[string]time.Time, len(s.retainedAssets))
	for name, deadline := range s.retainedAssets {
		retainedAssets[name] = deadline
	}
	targetDuration := max(2*m.settings.HlsSegmentDuration(), maximumFragmentDuration(window))
	timeline := m.timeline(info.Duration)
	targetSegment := timeline.locate(presentationTarget).segment
	playlistAnchor := max(targetSegment, s.playheadSegment)
	playlistTarget := presentationTarget
	if playlistAnchor != targetSegment {
		playlistTarget = timeline.segmentStart(playlistAnchor) + 0.001
	}
	windows, costs, _ = retainMaterializedWindows(windows, costs, playlistTarget, sideBytes)
	// A forward seek advances the existing live presentation. Keeping its
	// generation stable lets the attached HLS controller follow the new media
	// sequence without rebuilding the MediaSource or applying a second timeline
	// offset to segments whose timestamps are already absolute. Moving backward
	// still needs a fresh generation because an HLS media sequence cannot
	// decrease within one presentation.
	rotate := s.playlistTargetDuration > 0 && playlistAnchor < previousAnchor
	if !rotate {
		targetDuration = max(targetDuration, s.playlistTargetDuration)
	} else {
		assetVersion = fmt.Sprintf("%x", time.Now().UnixNano())
		discontinuitySequence = 0
	}
	advertised = max(advertised, advertisedEnd)
	sourceEnded = sourceEnded || reachedEnd
	if s.mediaInfo.Duration > 0 {
		sourceEnded = sourceEnded || advertised >= timeline.segmentCount()
	}
	allFragments := materializedFragmentsForTarget(windows, playlistTarget)
	if len(allFragments) == 0 {
		if direct && !reachedEnd {
			// A direct GOP can overlap an older materialized window and be
			// discarded before a later GOP reaches the target. Keep the useful
			// materialization, but leave the active playlist untouched until a
			// continuous target-covering presentation exists.
			s.materializedWindows = windows
			s.materializedBytes = costs
			s.materializedEnd = advertised
			s.sourceEnded = sourceEnded
			s.mtx.Unlock()
			return nil
		}
		s.mtx.Unlock()
		return fmt.Errorf("materialized HLS presentation does not cover %.3fs", playlistTarget)
	}
	playlistFragments := boundedPlaylistFragments(
		allFragments,
		playlistTarget,
		time.Duration(m.settings.HlsWindowSegments())*m.settings.HlsSegmentDuration(),
	)
	if len(playlistFragments) == 0 {
		s.mtx.Unlock()
		return fmt.Errorf("bounded HLS presentation does not cover %.3fs", playlistTarget)
	}
	if rotate {
		sequence = playlistAnchor
	} else {
		playlistFragments = legalForwardPlaylist(previousFragments, playlistFragments)
		cursor := advancePlaylistCursor(
			previousFragments,
			playlistFragments,
			playlistCursor{
				mediaSequence:         sequence,
				discontinuitySequence: discontinuitySequence,
			},
			playlistAnchor,
		)
		sequence = cursor.mediaSequence
		discontinuitySequence = cursor.discontinuitySequence
	}
	ended := materializedPlaylistReachedEnd(
		playlistFragments,
		info.Duration,
		sourceEnded,
		advertised,
		timeline.segmentCount(),
		m.settings.HlsSegmentDuration(),
	)
	retainedAssets = retainRemovedPlaylistAssets(
		retainedAssets,
		previousFragments,
		playlistFragments,
		time.Now(),
		2*m.settings.HlsSegmentDuration(),
	)
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
			playlistTarget,
			ended,
			playlistFragments,
		)
	}
	if err == nil && rotate && info.Duration <= 0 && ffmpeg.UsesTextSubtitles(info, selection) {
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
		s.materializedWindows = windows
		s.materializedBytes = costs
		s.assetVersion = assetVersion
		s.playlistTargetDuration = targetDuration
		s.publishedFragments = playlistFragments
		s.playlistAnchor = playlistAnchor
		s.retainedAssets = retainedAssets
		s.playlistSequence = sequence
		s.playlistDiscontinuitySequence = discontinuitySequence
		s.materializedEnd = advertised
		s.sourceEnded = sourceEnded
		s.mtx.Unlock()
	}
	return err
}

func materializedPlaylistReachedEnd(
	fragments []ffmpeg.HLSFragment,
	duration float64,
	sourceEnded bool,
	materializedEnd, total int,
	segmentDuration time.Duration,
) bool {
	if !sourceEnded || len(fragments) == 0 {
		return false
	}
	last := fragments[len(fragments)-1]
	if duration > 0 {
		return last.Start+last.Duration >= duration-0.25
	}
	if total > 0 {
		return materializedEnd >= total
	}
	return segmentDuration > 0 && last.Start+last.Duration >= float64(materializedEnd)*segmentDuration.Seconds()-0.25
}

func (m *manager) publishCachedPresentation(s *streamInfo, target playbackTarget, rotate bool) error {
	sideBytes := m.playbackCacheSideBytes()
	s.playlistMtx.Lock()
	defer s.playlistMtx.Unlock()

	s.mtx.Lock()
	if s.closing {
		s.mtx.Unlock()
		return nil
	}
	if rotate && fragmentsCoverTime(s.publishedFragments, target.sourceSeconds) {
		setPlaybackTargetLocked(s, target)
		s.mtx.Unlock()
		return nil
	}
	if !rotate {
		if target.segment <= s.playlistAnchor {
			s.mtx.Unlock()
			return nil
		}
		target.sourceSeconds = m.timeline(s.mediaInfo.Duration).segmentStart(target.segment) + 0.001
	}
	info := s.mediaInfo
	selection := s.selection
	sourceBitrate := info.Bitrate
	if sourceBitrate <= 0 {
		sourceBitrate = 8_000_000
	}
	costs := materializedWindowCosts(
		s.paths.outDir,
		s.materializedWindows,
		s.materializedBytes,
		sourceBitrate,
		ffmpeg.HLSReservationBitrate(info, selection),
	)
	windows, costs, _ := retainMaterializedWindows(s.materializedWindows, costs, target.sourceSeconds, sideBytes)
	fragments := boundedPlaylistFragments(
		materializedFragmentsForTarget(windows, target.sourceSeconds),
		target.sourceSeconds,
		time.Duration(m.settings.HlsWindowSegments())*m.settings.HlsSegmentDuration(),
	)
	if len(fragments) == 0 {
		s.mtx.Unlock()
		if rotate {
			return errors.New("cached HLS presentation is unavailable")
		}
		return nil
	}
	previous := append([]ffmpeg.HLSFragment(nil), s.publishedFragments...)
	if !rotate {
		fragments = legalForwardPlaylist(previous, fragments)
	}
	retained := make(map[string]time.Time, len(s.retainedAssets))
	for name, deadline := range s.retainedAssets {
		retained[name] = deadline
	}
	retained = retainRemovedPlaylistAssets(retained, previous, fragments, time.Now(), 2*m.settings.HlsSegmentDuration())
	ended := materializedPlaylistReachedEnd(
		fragments,
		info.Duration,
		s.sourceEnded,
		s.materializedEnd,
		m.timeline(info.Duration).segmentCount(),
		m.settings.HlsSegmentDuration(),
	)
	version := s.assetVersion
	targetDuration := s.playlistTargetDuration
	cursor := playlistCursor{
		mediaSequence:         s.playlistSequence,
		discontinuitySequence: s.playlistDiscontinuitySequence,
	}
	if rotate {
		version = fmt.Sprintf("%x", time.Now().UnixNano())
		targetDuration = max(2*m.settings.HlsSegmentDuration(), maximumFragmentDuration(fragments))
		cursor = playlistCursor{mediaSequence: target.segment}
	} else {
		cursor = advancePlaylistCursor(previous, fragments, cursor, target.segment)
	}
	fmp4 := s.directPlay && ffmpeg.UsesFMP4(info, selection)
	s.mtx.Unlock()

	writePlaylist := func() error {
		return ffmpeg.UpdateMaterializedHLS(
			s.paths.videoPlaylist,
			targetDuration,
			version,
			fmp4,
			cursor.mediaSequence,
			cursor.discontinuitySequence,
			target.sourceSeconds,
			ended,
			fragments,
		)
	}
	var err error
	if rotate {
		err = m.media.publishHls(
			estimatedHlsMetadataBytes(info.Duration, m.settings.HlsSegmentDuration(), selection.SubtitleTrackIndex >= 0),
			4,
			func() error {
				if err := ffmpeg.PrepareOnDemandHLS(
					s.paths.outDir,
					s.paths.videoPlaylist,
					s.paths.subtitlePlaylist,
					s.paths.masterPlaylist,
					info,
					selection,
					m.settings.HlsSegmentDuration(),
					m.settings.HlsWindowSegments(),
					version,
				); err != nil {
					return err
				}
				return writePlaylist()
			},
		)
	} else {
		err = writePlaylist()
	}
	if err != nil {
		return err
	}

	s.mtx.Lock()
	defer s.mtx.Unlock()
	if !rotate && target.segment <= s.playlistAnchor {
		return nil
	}
	s.materializedWindows = windows
	s.materializedBytes = costs
	s.publishedFragments = fragments
	s.playlistAnchor = target.segment
	s.retainedAssets = retained
	s.playlistSequence = cursor.mediaSequence
	s.playlistDiscontinuitySequence = cursor.discontinuitySequence
	if rotate {
		s.assetVersion = version
		setPlaybackTargetLocked(s, target)
		s.playlistTargetDuration = targetDuration
	}
	return nil
}

func (m *manager) publishCachedDemand(s *streamInfo, demand int) error {
	return m.publishCachedPresentation(s, playbackTarget{segment: demand}, false)
}

func maximumFragmentDuration(fragments []ffmpeg.HLSFragment) time.Duration {
	maximum := time.Duration(0)
	for _, fragment := range fragments {
		maximum = max(maximum, time.Duration(math.Ceil(fragment.Duration*float64(time.Second))))
	}
	return maximum
}

func materializedPresentationOrigin(windows map[int][]ffmpeg.HLSFragment, target float64) (float64, bool) {
	fragments := materializedFragmentsForTarget(windows, target)
	if len(fragments) == 0 {
		return 0, false
	}
	return fragments[0].Start, true
}

func materializedFragmentsForTarget(windows map[int][]ffmpeg.HLSFragment, target float64) []ffmpeg.HLSFragment {
	fragments := make([]ffmpeg.HLSFragment, 0)
	for _, window := range windows {
		fragments = append(fragments, window...)
	}
	sort.Slice(fragments, func(i, j int) bool { return fragments[i].Start < fragments[j].Start })
	anchor := -1
	for index, fragment := range fragments {
		if target >= fragment.Start-0.25 && target < fragment.Start+fragment.Duration-0.001 {
			anchor = index
			break
		}
	}
	if anchor < 0 {
		return nil
	}
	begin, end := anchor, anchor+1
	for begin > 0 && fragments[begin].Start <= fragments[begin-1].Start+fragments[begin-1].Duration+0.25 {
		begin--
	}
	for end < len(fragments) && fragments[end].Start <= fragments[end-1].Start+fragments[end-1].Duration+0.25 {
		end++
	}
	return append([]ffmpeg.HLSFragment(nil), fragments[begin:end]...)
}

func fragmentsCoverTime(fragments []ffmpeg.HLSFragment, target float64) bool {
	for _, fragment := range fragments {
		if target >= fragment.Start-0.25 && target < fragment.Start+fragment.Duration-0.001 {
			return true
		}
	}
	return false
}

func (m *manager) publishCachedTarget(s *streamInfo, target playbackTarget) error {
	return m.publishCachedPresentation(s, target, true)
}

func appendableDirectFragments(windows map[int][]ffmpeg.HLSFragment, incoming []ffmpeg.HLSFragment) []ffmpeg.HLSFragment {
	existing := make([]ffmpeg.HLSFragment, 0)
	for _, window := range windows {
		existing = append(existing, window...)
	}
	sort.Slice(incoming, func(i, j int) bool {
		return incoming[i].Start < incoming[j].Start
	})
	result := incoming[:0]
	for _, fragment := range incoming {
		coverage := make([]ffmpeg.HLSFragment, 0, len(existing)+len(result))
		coverage = append(coverage, existing...)
		coverage = append(coverage, result...)
		if !fragmentExtendsCoverage(fragment, coverage) {
			continue
		}
		result = append(result, fragment)
	}
	return result
}

func contiguousTranscodedFragments(fragments []ffmpeg.HLSFragment, begin, end int) ([]ffmpeg.HLSFragment, int) {
	byName := make(map[string]ffmpeg.HLSFragment, len(fragments))
	for _, fragment := range fragments {
		byName[fragment.Name] = fragment
	}
	contiguous := make([]ffmpeg.HLSFragment, 0, min(len(fragments), max(0, end-begin)))
	next := begin
	for next < end {
		fragment, ok := byName[videoSegmentName(next)]
		if !ok {
			break
		}
		contiguous = append(contiguous, fragment)
		next++
	}
	return contiguous, next
}

func transcodedWindowResult(fragments []ffmpeg.HLSFragment, reachedEnd bool) ffmpeg.VideoWindowResult {
	durations := make([]float64, 0, len(fragments))
	for _, fragment := range fragments {
		durations = append(durations, fragment.Duration)
	}
	return ffmpeg.VideoWindowResult{
		Generated:  len(fragments),
		Durations:  durations,
		ReachedEnd: reachedEnd,
	}
}

func fragmentExtendsCoverage(candidate ffmpeg.HLSFragment, fragments []ffmpeg.HLSFragment) bool {
	const tolerance = 0.001
	if candidate.Duration <= tolerance {
		return false
	}
	coverage := append([]ffmpeg.HLSFragment(nil), fragments...)
	sort.Slice(coverage, func(i, j int) bool {
		return coverage[i].Start < coverage[j].Start
	})
	cursor := candidate.Start
	end := candidate.Start + candidate.Duration
	for _, fragment := range coverage {
		fragmentEnd := fragment.Start + fragment.Duration
		if fragmentEnd <= cursor+tolerance {
			continue
		}
		if fragment.Start > cursor+tolerance {
			return true
		}
		cursor = max(cursor, fragmentEnd)
		if cursor >= end-tolerance {
			return false
		}
	}
	return cursor < end-tolerance
}

func nextUncoveredDirectSegment(windows map[int][]ffmpeg.HLSFragment, timeline playbackTimeline, begin, end int) int {
	if end <= 0 {
		end = math.MaxInt
	}
	for begin < end && timeline.containsSegment(begin) && materializedSegmentCovered(windows, timeline, begin) {
		begin++
	}
	return begin
}

func materializedSegmentCovered(windows map[int][]ffmpeg.HLSFragment, timeline playbackTimeline, index int) bool {
	if index < 0 || !timeline.containsSegment(index) {
		return false
	}
	start := timeline.segmentStart(index)
	end := timeline.segmentEnd(index)
	fragments := materializedFragmentsForTarget(windows, start+0.001)
	if len(fragments) == 0 || fragments[0].Start > start+0.25 {
		return false
	}
	coveredEnd := fragments[0].Start + fragments[0].Duration
	for _, fragment := range fragments[1:] {
		coveredEnd = max(coveredEnd, fragment.Start+fragment.Duration)
	}
	return coveredEnd >= end-0.25
}

func (m *manager) switchToTranscode(s *streamInfo, current *segmentJob) error {
	s.playlistMtx.Lock()
	defer s.playlistMtx.Unlock()

	s.mtx.Lock()
	if !s.directPlay {
		s.mtx.Unlock()
		return nil
	}
	s.directPlay = false
	s.retainedAssets = retainRemovedPlaylistAssets(
		s.retainedAssets,
		s.publishedFragments,
		nil,
		time.Now(),
		2*m.settings.HlsSegmentDuration(),
	)
	s.materializedWindows = make(map[int][]ffmpeg.HLSFragment)
	s.materializedBytes = make(map[int]int64)
	s.publishedFragments = nil
	s.playlistTargetDuration = 0
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
	materializedEnd := s.materializedEnd
	sourceEnded := s.sourceEnded
	s.mtx.Unlock()

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
	if err == nil && info.Duration <= 0 && ffmpeg.UsesTextSubtitles(info, selection) {
		err = ffmpeg.UpdateProgressiveSubtitleHLS(
			s.paths.subtitlePlaylist,
			m.settings.HlsSegmentDuration(),
			m.settings.HlsWindowSegments(),
			s.assetVersion,
			progressiveSubtitles,
			progressiveLast,
			sourceEnded && progressiveSubtitles >= materializedEnd,
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
		actual = m.timeline(0).segmentStart(job.materializationBegin)
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
	ended := s.sourceEnded && count >= s.materializedEnd
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
	timeline := m.timeline(s.mediaDuration())
	hasTarget := targetSeconds >= 0 && !math.IsNaN(targetSeconds) && !math.IsInf(targetSeconds, 0)
	var target playbackTarget
	if hasTarget {
		target = timeline.locate(targetSeconds)
	}
	s.mtx.Lock()
	requiresSubtitles := hasTarget && ffmpeg.UsesTextSubtitles(s.mediaInfo, s.selection)
	videoReady := hasTarget && directFragmentsCoverTime(s.materializedWindows, target.sourceSeconds)
	s.mtx.Unlock()
	subtitlesReady := !requiresSubtitles || m.media.touchHls(filepath.Join(s.paths.outDir, subtitleSegmentName(target.segment)))
	if requiresSubtitles && videoReady && !subtitlesReady {
		m.requestSelectedSubtitleTarget(s, target)
	}
	status := s.playbackStatus(
		targetSeconds,
		timeline,
		time.Now(),
		stats.ActivePeers,
		stats.TotalPeers,
		subtitlesReady,
	)
	return status, nil
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func directFragmentsCoverTime(windows map[int][]ffmpeg.HLSFragment, target float64) bool {
	for _, fragments := range windows {
		if fragmentsCoverTime(fragments, target) {
			return true
		}
	}
	return false
}

func classifyHlsStatus(status domain.HlsStatus, now time.Time) domain.HlsStatus {
	if status.Phase == domain.HlsPhaseProbing || status.Phase == domain.HlsPhasePreparing {
		idle := now.Sub(status.LastProgress)
		switch status.Stage {
		case domain.HlsStageWaitingSource, domain.HlsStageSourceBlocked:
			if status.ActivePeers == 0 && idle >= 5*time.Second {
				status.Phase = domain.HlsPhaseNoPeers
				status.Message = "No active peers have the required torrent pieces; discovery is still running"
			} else if idle >= 15*time.Second {
				status.Phase = domain.HlsPhaseStalled
				status.Message = "The required torrent pieces have not advanced recently"
			}
		case domain.HlsStageWaitingCPU:
			status.Message = "Waiting for foreground media-worker capacity"
		case domain.HlsStagePackaging:
			if idle >= 15*time.Second {
				status.Phase = domain.HlsPhaseStalled
				status.Message = "The media packager has not produced a complete fragment recently"
			}
		case domain.HlsStagePublishing:
			status.Message = "Publishing a complete fragment to the HLS presentation"
		}
	}
	return status
}

// requestSelectedSubtitleTarget makes the selected text track part of the
// presentation readiness contract. It starts after the target video is
// published. Admission prioritizes it over background work without coupling
// video cache progress to a potentially slow subtitle source read.
func (m *manager) requestSelectedSubtitleTarget(s *streamInfo, target playbackTarget) {
	if s == nil || target.segment < 0 || m.media == nil {
		return
	}
	path := filepath.Join(s.paths.outDir, subtitleSegmentName(target.segment))
	if m.media.touchHls(path) {
		return
	}

	s.mtx.Lock()
	if s.closing || s.fatalErr != nil || !ffmpeg.UsesTextSubtitles(s.mediaInfo, s.selection) ||
		!directFragmentsCoverTime(s.materializedWindows, target.sourceSeconds) {
		s.mtx.Unlock()
		return
	}
	if err := validateSegmentIndex(m.timeline(s.mediaInfo.Duration), s.materializedEnd, target.segment, "subtitle"); err != nil {
		s.mtx.Unlock()
		return
	}
	claimSubtitleTargetLocked(s.subtitleJobs, target.segment)
	job, jobCtx, created, err := s.acquireJobLocked(
		subtitleSegmentJob,
		target.segment,
		target.segment,
		target.segment+1,
		false,
		m.scheduler,
		m.settings.MaxJobsPerStream(),
	)
	s.mtx.Unlock()
	if err != nil {
		// Admission is transient. Status polling retries without converting a
		// busy worker pool into a terminal playback error.
		if !errors.Is(err, errStreamJobQueueFull) && !errors.Is(err, errStreamJobLimit) && !errors.Is(err, context.Canceled) {
			s.markSegmentError(target.segment, err)
		}
		return
	}
	if created {
		go m.runSubtitleJob(s, jobCtx, job)
	}
}

func (m *manager) ensureSubtitleSegment(ctx context.Context, s *streamInfo, index int) error {
	s.mtx.Lock()
	streamErr := s.availabilityErrorLocked()
	info := s.mediaInfo
	selection := s.selection
	materializedEnd := s.materializedEnd
	s.mtx.Unlock()
	if streamErr != nil {
		return streamErr
	}
	if !ffmpeg.UsesTextSubtitles(info, selection) {
		return errors.New("stream has no text subtitle rendition")
	}
	path := filepath.Join(s.paths.outDir, subtitleSegmentName(index))
	if m.media.touchHls(path) {
		return nil
	}
	if err := validateSegmentIndex(m.timeline(info.Duration), materializedEnd, index, "subtitle"); err != nil {
		return err
	}
	// A requested segment of the selected subtitle rendition is required
	// playback data. Wait until its video is published, then reserve the cue as
	// foreground work. Scheduler admission preempts background work if needed;
	// the subtitle read itself must not pause the byte-based video horizon.
	for {
		s.mtx.Lock()
		if err := s.availabilityErrorLocked(); err != nil {
			s.mtx.Unlock()
			return err
		}
		target := m.timeline(s.mediaInfo.Duration).locate(m.timeline(0).segmentStart(index))
		videoReady := directFragmentsCoverTime(s.materializedWindows, target.sourceSeconds)
		if !videoReady {
			videoJob := findSegmentJob(s.videoJobs, index)
			s.mtx.Unlock()
			if videoJob == nil {
				return domain.ErrHlsTemporarilyUnavailable
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-videoJob.done:
				if videoJob.err != nil && !errors.Is(videoJob.err, context.Canceled) {
					return videoJob.err
				}
			case <-videoJob.progress:
			}
			continue
		}
		job, jobCtx, created, err := s.acquireJobLocked(subtitleSegmentJob, index, index, index+1, false, m.scheduler, m.settings.MaxJobsPerStream())
		if err != nil {
			var changed <-chan struct{}
			if errors.Is(err, errStreamJobLimit) {
				changed = m.scheduler.jobChanges()
			}
			s.mtx.Unlock()
			switch {
			case errors.Is(err, errStreamJobQueueFull):
				if waitErr := m.scheduler.waitForJobCapacity(ctx); waitErr != nil {
					return waitErr
				}
				continue
			case errors.Is(err, errStreamJobLimit):
				if waitErr := waitForJobChange(ctx, changed); waitErr != nil {
					return waitErr
				}
				continue
			}
			return fmt.Errorf("%w: %v", domain.ErrHlsTemporarilyUnavailable, err)
		}
		if created {
			go m.runSubtitleJob(s, jobCtx, job)
		}
		job.waiters++
		s.mtx.Unlock()
		waitErr := waitForGeneratedAsset(ctx, m.media, path, job)
		s.releaseJobWaiter(job, waitErr)
		if errors.Is(waitErr, context.Canceled) && ctx.Err() == nil && s.ctx != nil && s.ctx.Err() == nil {
			return domain.ErrHlsTemporarilyUnavailable
		}
		return waitErr
	}
}

func (m *manager) runSubtitleJob(s *streamInfo, ctx context.Context, job *segmentJob) {
	defer func() {
		if recovered := recover(); recovered != nil {
			job.err = fmt.Errorf("subtitle generation panic: %v", recovered)
		}
		s.completeJob(subtitleSegmentJob, job)
		m.enforceCacheLimit()
		if job.err == nil {
			s.mtx.Lock()
			demand := s.playheadSegment
			s.mtx.Unlock()
			m.prefetchProgressiveWindow(s, demand)
		}
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
	ctx, stopResourceGuard, finishResourceGuard := m.guardHlsJobResources(ctx, job)
	defer finishResourceGuard()
	job.err = m.runBoundedPackager(ctx, s, job, mediaPackagerWorker, func(runCtx context.Context) error {
		s.markJobStarted(job)
		for index := job.begin; index < job.end; index++ {
			path := filepath.Join(s.paths.outDir, subtitleSegmentName(index))
			if m.media.touchHls(path) {
				continue
			}
			if err := ffmpeg.GenerateSubtitleSegment(
				runCtx,
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
		if index >= job.begin && index < job.end && !jobFinished(job) && (job.ctx == nil || job.ctx.Err() == nil) {
			return job
		}
	}
	return nil
}

func segmentJobID(kind string, begin, end int, started time.Time) string {
	return fmt.Sprintf("%s-%d-%d-%d", kind, begin, end, started.UnixNano())
}

func claimVideoTargetLocked(jobs map[*segmentJob]struct{}, begin, end int, targetSeconds float64) {
	for job := range jobs {
		overlaps := job.begin < end && begin < job.end
		if overlaps {
			job.targetSeconds = targetSeconds
			continue
		}
		if jobFinished(job) {
			continue
		}
		retireSegmentJobLocked(jobs, job)
	}
}

func claimSubtitleTargetLocked(jobs map[*segmentJob]struct{}, index int) {
	for job := range jobs {
		if index >= job.begin && index < job.end {
			continue
		}
		if !jobFinished(job) {
			retireSegmentJobLocked(jobs, job)
		}
	}
}

func retargetVideoJobsLocked(jobs map[*segmentJob]struct{}, targetSeconds float64) {
	for job := range jobs {
		if !jobFinished(job) {
			job.targetSeconds = targetSeconds
		}
	}
}

func (s *streamInfo) releaseJobWaiter(job *segmentJob, waitErr error) {
	s.mtx.Lock()
	if job.waiters > 0 {
		job.waiters--
	}
	// Once FFmpeg has started, keep producing the bounded canonical window even
	// if a browser or reverse proxy closes its HTTP request. A retry can then
	// join the same job instead of throwing away minutes of torrent/CPU work.
	if waitErr != nil && job.waiters == 0 && !job.background && !job.started && !jobFinished(job) {
		if _, video := s.videoJobs[job]; video {
			retireSegmentJobLocked(s.videoJobs, job)
		} else {
			retireSegmentJobLocked(s.subtitleJobs, job)
		}
	}
	s.mtx.Unlock()
}

func (s *streamInfo) markJobStarted(job *segmentJob) {
	s.mtx.Lock()
	job.started = true
	job.lastProgress = time.Now()
	s.mtx.Unlock()
}

type mediaWorkerKind uint8

const (
	mediaPackagerWorker mediaWorkerKind = iota
	mediaEncoderWorker
)

func (m *manager) runBoundedPackager(ctx context.Context, s *streamInfo, job *segmentJob, worker mediaWorkerKind, run func(context.Context) error) error {
	packagerDeadline := max(10*time.Second, 3*m.settings.HlsSegmentDuration())
	if worker == mediaEncoderWorker {
		// Compatibility transcoding can legitimately need longer than real time
		// for its first frame on a small VPS.
		packagerDeadline = max(packagerDeadline, time.Minute)
	}
	const sourceStallObservation = 500 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		attempt := func() error {
			s.markJobStage(job, domain.HlsStagePackaging)
			attemptCtx, stop := context.WithCancelCause(ctx)
			monitorDone := make(chan struct{})
			go func() {
				defer close(monitorDone)
				ticker := time.NewTicker(250 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-attemptCtx.Done():
						return
					case now := <-ticker.C:
						sourceProgress, outputProgress, diagnosticProgress, rangeStart, rangeEnd := s.packagerProgress(job)
						missing, _ := s.source.rangePieceCounts(rangeStart, rangeEnd-rangeStart)
						if rangeEnd > rangeStart && missing > 0 &&
							now.Sub(sourceProgress) >= sourceStallObservation &&
							now.Sub(outputProgress) >= sourceStallObservation {
							s.markJobStage(job, domain.HlsStageSourceBlocked)
							stop(errSourceBlocked)
							return
						}
						lastProgress := outputProgress
						if sourceProgress.After(lastProgress) {
							lastProgress = sourceProgress
						}
						if diagnosticProgress.After(lastProgress) {
							lastProgress = diagnosticProgress
						}
						if now.Sub(lastProgress) >= packagerDeadline {
							stop(errPackagerNoOutput)
							return
						}
					}
				}
			}()
			err := run(attemptCtx)
			stop(context.Canceled)
			<-monitorDone
			if cause := context.Cause(attemptCtx); errors.Is(cause, errSourceBlocked) || errors.Is(cause, errPackagerNoOutput) {
				return cause
			}
			return err
		}

		var err error
		s.markJobStage(job, domain.HlsStageWaitingCPU)
		switch worker {
		case mediaPackagerWorker:
			err = m.scheduler.packageMedia(ctx, job.background, job.cancel, attempt)
		case mediaEncoderWorker:
			err = m.scheduler.transcode(ctx, job.background, job.cancel, attempt)
		default:
			return fmt.Errorf("unknown media worker kind %d", worker)
		}
		if !errors.Is(err, errSourceBlocked) {
			return err
		}

		_, _, _, rangeStart, rangeEnd := s.packagerProgress(job)
		if rangeEnd <= rangeStart {
			return err
		}
		s.markJobStage(job, domain.HlsStageWaitingSource)
		log.Printf("HLS packager yielded media worker while waiting for torrent range: dir=%s job=%s range=[%d,%d)", filepath.Base(s.paths.outDir), job.id, rangeStart, rangeEnd)
		if waitErr := s.source.WaitRange(ctx, rangeStart, rangeEnd-rangeStart); waitErr != nil {
			return waitErr
		}
	}
}

func waitForGeneratedAsset(ctx context.Context, cache *mediaCache, path string, job *segmentJob) error {
	return waitForJobProgress(ctx, job, func() bool {
		return cache.touchHls(path)
	}, "HLS asset was not generated")
}

func waitForDirectTarget(ctx context.Context, s *streamInfo, job *segmentJob) error {
	return waitForJobProgress(ctx, job, func() bool {
		s.mtx.Lock()
		defer s.mtx.Unlock()
		return directFragmentsCoverTime(s.materializedWindows, job.targetSeconds)
	}, "HLS target was not generated")
}

func waitForJobProgress(ctx context.Context, job *segmentJob, ready func() bool, missing string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-job.done:
			if ready() {
				return nil
			}
			if job.err != nil {
				return job.err
			}
			return errors.New(missing)
		case <-job.progress:
		}
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
