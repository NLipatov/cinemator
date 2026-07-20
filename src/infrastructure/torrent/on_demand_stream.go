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
		s.progressiveAdvertised = target.segment
		if ffmpeg.UsesTextSubtitles(info, s.selection) && info.Duration > 0 {
			s.progressiveSubtitles = m.timeline(info.Duration).segmentCount()
			s.progressiveLast = info.Duration - m.timeline(0).segmentStart(s.progressiveSubtitles-1)
		} else {
			s.progressiveSubtitles = target.segment
		}
		s.playbackWindow = target.windowBegin
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
		if assetName != "master.m3u8" && version != currentVersion {
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

// unpublishActiveHlsAssets rotates one presentation generation so the cache
// coordinator can evict the selected immutable assets. The caller holds
// generationMtx from selection through physical eviction.
func (m *manager) unpublishActiveHlsAssets(stream *streamInfo, items []hlsCacheItem) error {
	if len(items) == 0 {
		return nil
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
			owner, _ := m.timeline(0).windowForSegment(index)
			evictedOwners[owner] = struct{}{}
		}
	}
	stream.mtx.Lock()
	if stream.closing || stream.fatalErr != nil || stream.assetVersion == "" {
		stream.mtx.Unlock()
		return nil
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
	progressiveSequence := m.timeline(info.Duration).locate(presentationTarget).segment
	progressiveDiscontinuitySequence := 0
	progressiveLast := stream.progressiveLast
	progressiveEnded := stream.progressiveEnded
	directWindows := make(map[int][]ffmpeg.HLSFragment, len(stream.directWindows))
	for owner, window := range stream.directWindows {
		if _, evicted := evictedOwners[owner]; evicted {
			continue
		}
		directWindows[owner] = window
	}
	fragments := materializedFragmentsForTarget(directWindows, presentationTarget)
	stream.mtx.Unlock()
	err := m.media.publishHls(
		estimatedHlsMetadataBytes(info.Duration, m.settings.HlsSegmentDuration(), selection.SubtitleTrackIndex >= 0),
		4,
		func() error {
			err := ffmpeg.PrepareOnDemandHLS(
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
			if err != nil {
				return err
			}
			if err = ffmpeg.UpdateMaterializedHLS(
				stream.paths.videoPlaylist,
				materializedTarget,
				version,
				directPlay && ffmpeg.UsesFMP4(info, selection),
				progressiveSequence,
				progressiveDiscontinuitySequence,
				presentationTarget,
				progressiveEnded,
				fragments,
			); err != nil {
				return err
			}
			if info.Duration <= 0 && ffmpeg.UsesTextSubtitles(info, selection) {
				return ffmpeg.UpdateProgressiveSubtitleHLS(
					stream.paths.subtitlePlaylist,
					m.settings.HlsSegmentDuration(),
					m.settings.HlsWindowSegments(),
					version,
					progressiveSubtitles,
					progressiveLast,
					progressiveEnded && progressiveSubtitles >= progressiveCount,
				)
			}
			return nil
		},
	)
	stream.mtx.Lock()
	if err != nil {
		stream.fatalErr = fmt.Errorf("rotate HLS generation: %w", err)
		stream.status.Phase = domain.HlsPhaseError
		stream.status.Message = publicStreamError(stream.fatalErr)
		stream.status.LastProgress = time.Now()
		stream.mtx.Unlock()
		return stream.fatalErr
	}
	stream.directWindows = directWindows
	stream.progressiveSequence = progressiveSequence
	stream.progressiveDiscontinuitySequence = progressiveDiscontinuitySequence
	stream.materializedTarget = materializedTarget
	stream.assetVersion = version
	stream.mtx.Unlock()
	return nil
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
	staleVersion := assetName != "master.m3u8" && version != currentVersion
	if strings.HasSuffix(assetName, ".m3u8") {
		if staleVersion {
			return domain.ErrHlsPlaylistChanged
		}
		if assetName == filepath.Base(s.paths.videoPlaylist) {
			s.mtx.Lock()
			needsStartup := s.materializedTarget <= 0
			s.mtx.Unlock()
			if needsStartup {
				m.prefetchProgressiveWindow(s, -1)
			}
		} else if assetName == "subs.m3u8" {
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
		if staleVersion {
			if m.media.touchHls(path) {
				return nil
			}
			return domain.ErrHlsPlaylistChanged
		}
		s.mtx.Lock()
		s.playbackWindow = owner
		s.mtx.Unlock()
		if m.media.touchHls(path) {
			s.mtx.Lock()
			requested := directPrefetchIndexLocked(s, owner, assetName)
			s.mtx.Unlock()
			m.prefetchProgressiveWindow(s, requested)
			return nil
		}
		s.mtx.Lock()
		delete(s.directWindows, owner)
		s.mtx.Unlock()
		if err := m.ensureVideoSegment(ctx, s, owner); err != nil {
			return err
		}
		if !m.media.touchHls(path) {
			return domain.ErrHlsPlaylistChanged
		}
		s.mtx.Lock()
		requested := directPrefetchIndexLocked(s, owner, assetName)
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
		s.mtx.Lock()
		s.playbackWindow = begin
		s.mtx.Unlock()
		if m.media.touchHls(path) {
			m.prefetchProgressiveWindow(s, index)
			return nil
		}
		s.mtx.Lock()
		delete(s.directWindows, begin)
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
	s.mtx.Lock()
	if s.closing || s.fatalErr != nil {
		s.mtx.Unlock()
		return
	}
	currentPresentation := materializedFragmentsForTarget(s.directWindows, s.presentationTarget)
	if fragmentsCoverTime(currentPresentation, target.sourceSeconds) {
		s.presentationTarget = target.sourceSeconds
		s.mtx.Unlock()
		s.markReady(target.segment)
		return
	}
	if directFragmentsCoverTime(s.directWindows, target.sourceSeconds) {
		s.mtx.Unlock()
		if err := m.publishCachedTarget(s, target); err != nil {
			s.markSegmentError(target.segment, err)
			return
		}
		s.markReady(target.segment)
		return
	}
	timeline := m.timeline(s.mediaInfo.Duration)
	if !timeline.containsSegment(target.segment) {
		s.mtx.Unlock()
		s.markSegmentError(target.segment, errors.New("video segment out of range"))
		return
	}
	begin := target.segment
	end := begin + 1
	if total := timeline.segmentCount(); total > 0 {
		end = min(end, total)
	}
	job, jobCtx, created, err := s.acquireJobLocked(videoSegmentJob, target.segment, begin, end, false, m.scheduler, m.settings.MaxJobsPerStream())
	if err != nil {
		s.mtx.Unlock()
		s.markSegmentError(target.segment, err)
		return
	}
	s.presentationTarget = target.sourceSeconds
	s.progressiveDemand = target.segment
	if created {
		job.targetSeconds = target.sourceSeconds
	}
	s.mtx.Unlock()
	if created {
		go m.runVideoJob(s, jobCtx, job)
	}
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
	if m.media.touchHls(path) {
		s.markPreparing(index, m.timeline(0).segmentStart(index))
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
		timeline := m.timeline(s.mediaInfo.Duration)
		total = timeline.segmentCount()
		if !timeline.containsSegment(index) {
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

	s.markPreparing(index, m.timeline(0).segmentStart(index))
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
		begin, _ := m.timeline(s.mediaInfo.Duration).windowForSegment(index)
		if len(s.directWindows[begin]) > 0 {
			s.mtx.Unlock()
			s.markReady(index)
			return nil
		}
	}
	if total <= 0 {
		total = s.progressiveAdvertised
	}
	timeline := m.timeline(s.mediaInfo.Duration)
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
		waitErr = waitForJob(ctx, job)
	} else {
		waitErr = waitForGeneratedAsset(ctx, m.media, path, job)
	}
	s.releaseJobWaiter(job, waitErr)
	if waitErr == nil {
		s.markReady(index)
		m.prefetchProgressiveWindow(s, index)
	}
	return waitErr
}

// prefetchProgressiveWindow restores a bounded materialized horizon. Startup
// publishes one fragment; subsequent foreground work reaches the low watermark
// before preemptible work grows toward the target.
func (m *manager) prefetchProgressiveWindow(s *streamInfo, requested int) {
	window := m.settings.HlsWindowSegments()
	s.mtx.Lock()
	if requested >= 0 {
		s.progressiveDemand = requested
	}
	if s.closing || s.progressiveEnded || s.progressiveRetry {
		s.mtx.Unlock()
		return
	}
	for job := range s.videoJobs {
		if !jobFinished(job) {
			s.mtx.Unlock()
			return
		}
	}
	begin := s.progressiveAdvertised
	timeline := m.timeline(s.mediaInfo.Duration)
	if s.directPlay {
		begin = max(begin, nextDirectWindowBegin(s.directWindows, m.timeline(0)))
		begin = nextUncoveredDirectSegment(s.directWindows, timeline, begin, timeline.segmentCount())
	}
	total := 0
	if s.mediaInfo.Duration > 0 {
		total = timeline.segmentCount()
		if begin >= total {
			s.progressiveEnded = true
			s.mtx.Unlock()
			return
		}
	}
	plan := progressivePrefetchPlan(s.progressiveDemand, begin, total, window, m.settings.HlsSegmentDuration(), s.progressiveTarget, requested < 0)
	if !plan.ok {
		s.mtx.Unlock()
		return
	}
	job, jobCtx, created, err := s.acquireJobLocked(videoSegmentJob, begin, begin, plan.end, plan.background, m.scheduler, m.settings.MaxJobsPerStream())
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

type progressivePlan struct {
	end        int
	background bool
	ok         bool
}

func progressivePrefetchPlan(demand, advertised, total, maximum int, segmentDuration, targetReserve time.Duration, startup bool) progressivePlan {
	if startup {
		end := advertised + 1
		if total > 0 {
			end = min(end, total)
		}
		return progressivePlan{end: end, ok: end > advertised}
	}
	seconds := max(1.0, segmentDuration.Seconds())
	low := max(1, int(math.Ceil(15/seconds)))
	targetSeconds := min(60.0, max(30.0, targetReserve.Seconds()))
	target := max(low, int(math.Ceil(targetSeconds/seconds)))
	high := max(target, int(math.Ceil(60/seconds)))
	ahead := advertised - max(0, demand)
	if ahead >= target {
		return progressivePlan{}
	}
	background := ahead >= low
	desired := demand + target
	if !background {
		desired = demand + low
	}
	end := min(advertised+max(1, maximum), desired)
	end = min(end, demand+high)
	if total > 0 {
		end = min(end, total)
	}
	return progressivePlan{end: end, background: background, ok: end > advertised}
}

func directPrefetchIndexLocked(s *streamInfo, owner int, assetName string) int {
	fragments := s.directWindows[owner]
	if len(fragments) > 0 && fragments[len(fragments)-1].Name == assetName {
		return max(owner, s.progressiveAdvertised-1)
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

func (m *manager) prefetchProgressiveSubtitles(s *streamInfo, begin, end int) {
	s.mtx.Lock()
	if s.closing || !ffmpeg.UsesTextSubtitles(s.mediaInfo, s.selection) {
		s.mtx.Unlock()
		return
	}
	if s.mediaInfo.Duration <= 0 {
		begin = min(begin, s.progressiveSubtitles)
	}
	end = min(end, s.progressiveAdvertised)
	end = min(end, begin+m.settings.HlsWindowSegments())
	if begin >= end {
		s.mtx.Unlock()
		return
	}
	job, jobCtx, created, err := s.acquireJobLocked(subtitleSegmentJob, begin, begin, end, true, m.scheduler, m.settings.MaxJobsPerStream())
	if err != nil {
		s.mtx.Unlock()
		return
	}
	if !created {
		s.mtx.Unlock()
		return
	}
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
		s.finishJob(videoSegmentJob, job)
		m.enforceCacheLimit()
		if job.err == nil {
			s.recordJobDelivery(job, m.settings.HlsSegmentDuration(), time.Now())
			s.mtx.Lock()
			demand := s.progressiveDemand
			s.mtx.Unlock()
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

	transcodedFragments := make([]ffmpeg.HLSFragment, 0, segmentCount)
	generateTranscoded := func(runCtx context.Context) error {
		transcodedFragments = transcodedFragments[:0]
		job.result, job.err = ffmpeg.GenerateVideoWindow(
			runCtx,
			s.source.URLForJob(job.id),
			s.paths.outDir,
			info,
			selection,
			job.begin,
			job.end-job.begin,
			m.settings.HlsSegmentDuration(),
			func(index int, duration float64) error {
				fragment := ffmpeg.HLSFragment{
					Start:    m.timeline(0).segmentStart(index),
					Duration: duration,
					Name:     videoSegmentName(index),
				}
				transcodedFragments = append(transcodedFragments, fragment)
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
		return job.err
	}
	if directPlay {
		var direct ffmpeg.DirectWindowResult
		pending := make([]ffmpeg.HLSFragment, 0, 1)
		published := false
		publishFragment := func(fragment ffmpeg.HLSFragment) error {
			pending = append(pending, fragment)
			if !published {
				s.mtx.Lock()
				covered := directFragmentsCoverTime(s.directWindows, job.targetSeconds)
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
				segmentCount,
				m.settings.HlsSegmentDuration(),
				prerollBudget,
				publishFragment,
			)
			return err
		}
		var directErr error
		if ffmpeg.CopiesAudio(info, selection) {
			directErr = m.runBoundedPackager(ctx, s, job, false, generateDirect)
		} else {
			directErr = m.runBoundedPackager(ctx, s, job, true, generateDirect)
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
				job.err = m.runBoundedPackager(ctx, s, job, true, generateTranscoded)
			}
		} else {
			job.err = directErr
		}
	} else {
		job.err = m.runBoundedPackager(ctx, s, job, true, generateTranscoded)
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

func (m *manager) maybeRefineStreamDuration(s *streamInfo) {
	targetAhead := max(1, int(math.Ceil(30/max(1.0, m.settings.HlsSegmentDuration().Seconds()))))
	s.mtx.Lock()
	if s.closing || s.durationRefinementStarted || s.progressiveAdvertised-s.progressiveDemand < targetAhead {
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

func (m *manager) publishDirectWindow(s *streamInfo, job *segmentJob) error {
	s.mtx.Lock()
	fmp4 := ffmpeg.UsesFMP4(s.mediaInfo, s.selection)
	s.mtx.Unlock()
	return m.publishMaterializedWindow(s, job, job.fragments, fmp4, true, job.end, job.directEnd)
}

func (m *manager) publishTranscodedWindow(s *streamInfo, job *segmentJob) error {
	fragments := make([]ffmpeg.HLSFragment, 0, len(job.result.Durations))
	cursor := m.timeline(0).segmentStart(job.begin)
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
		existing := append([]ffmpeg.HLSFragment(nil), windows[job.begin]...)
		delete(windows, job.begin)
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
	windows[job.begin] = window
	sequence := s.progressiveSequence
	discontinuitySequence := s.progressiveDiscontinuitySequence
	advertised := s.progressiveAdvertised
	ended := s.progressiveEnded
	assetVersion := s.assetVersion
	presentationTarget := job.targetSeconds
	info := s.mediaInfo
	selection := s.selection
	progressiveSubtitles := s.progressiveSubtitles
	progressiveLast := s.progressiveLast
	targetDuration := max(2*m.settings.HlsSegmentDuration(), maximumFragmentDuration(window))
	targetSegment := m.timeline(info.Duration).locate(presentationTarget).segment
	rotate := s.materializedTarget > 0 && (targetDuration > s.materializedTarget || sequence != targetSegment)
	if !rotate {
		targetDuration = max(targetDuration, s.materializedTarget)
	} else {
		assetVersion = fmt.Sprintf("%x", time.Now().UnixNano())
	}
	sequence = targetSegment
	discontinuitySequence = 0
	advertised = max(advertised, advertisedEnd)
	ended = ended || reachedEnd
	if s.mediaInfo.Duration > 0 {
		ended = ended || advertised >= m.timeline(s.mediaInfo.Duration).segmentCount()
	}
	allFragments := materializedFragmentsForTarget(windows, presentationTarget)
	if len(allFragments) == 0 {
		s.mtx.Unlock()
		return fmt.Errorf("materialized HLS presentation does not cover %.3fs", presentationTarget)
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
	s.playlistMtx.Lock()
	defer s.playlistMtx.Unlock()

	s.mtx.Lock()
	fragments := materializedFragmentsForTarget(s.directWindows, target.sourceSeconds)
	if len(fragments) == 0 {
		s.mtx.Unlock()
		return errors.New("cached HLS presentation is unavailable")
	}
	version := fmt.Sprintf("%x", time.Now().UnixNano())
	info := s.mediaInfo
	selection := s.selection
	targetDuration := max(2*m.settings.HlsSegmentDuration(), maximumFragmentDuration(fragments))
	ended := s.progressiveEnded
	fmp4 := s.directPlay && ffmpeg.UsesFMP4(info, selection)
	s.mtx.Unlock()

	err := m.media.publishHls(
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
			return ffmpeg.UpdateMaterializedHLS(
				s.paths.videoPlaylist,
				targetDuration,
				version,
				fmp4,
				target.segment,
				0,
				target.sourceSeconds,
				ended,
				fragments,
			)
		},
	)
	if err != nil {
		return err
	}
	s.mtx.Lock()
	s.assetVersion = version
	s.presentationTarget = target.sourceSeconds
	s.progressiveSequence = target.segment
	s.progressiveDiscontinuitySequence = 0
	s.materializedTarget = targetDuration
	s.mtx.Unlock()
	return nil
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
		var ok bool
		fragment, ok = trimMaterializedOverlap(fragment, existing)
		if !ok {
			continue
		}
		fragment, ok = trimMaterializedOverlap(fragment, result)
		if !ok {
			continue
		}
		result = append(result, fragment)
	}
	return result
}

func trimMaterializedOverlap(candidate ffmpeg.HLSFragment, fragments []ffmpeg.HLSFragment) (ffmpeg.HLSFragment, bool) {
	const tolerance = 0.001
	for _, fragment := range fragments {
		start := candidate.Start
		end := start + candidate.Duration
		otherStart := fragment.Start
		otherEnd := otherStart + fragment.Duration
		if start >= otherEnd-tolerance || otherStart >= end-tolerance {
			continue
		}
		if overlap := otherEnd - start; start >= otherStart && overlap <= 0.25 {
			candidate.Start = otherEnd
			continue
		}
		if overlap := end - otherStart; end <= otherEnd && overlap <= 0.25 {
			candidate.Duration -= overlap
			continue
		}
		return ffmpeg.HLSFragment{}, false
	}
	return candidate, candidate.Duration > tolerance
}

func nextDirectWindowBegin(windows map[int][]ffmpeg.HLSFragment, timeline playbackTimeline) int {
	end := 0.0
	for _, window := range windows {
		for _, fragment := range window {
			end = max(end, fragment.Start+fragment.Duration)
		}
	}
	return timeline.locate(end + 0.001).segment
}

func nextUncoveredDirectSegment(windows map[int][]ffmpeg.HLSFragment, timeline playbackTimeline, begin, end int) int {
	if end <= 0 {
		end = math.MaxInt
	}
	for begin < end && timeline.containsSegment(begin) && directFragmentsCoverTime(windows, timeline.segmentEnd(begin)-0.01) {
		begin++
	}
	return begin
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
	if err == nil && info.Duration <= 0 && ffmpeg.UsesTextSubtitles(info, selection) {
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
		actual = m.timeline(0).segmentStart(job.begin)
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
	status := s.playbackStatus(
		targetSeconds,
		m.timeline(s.mediaDuration()),
		time.Now(),
		stats.BytesReadUsefulData.Int64(),
		stats.ActivePeers,
		stats.TotalPeers,
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
		for _, fragment := range fragments {
			if target >= fragment.Start-0.25 && target < fragment.Start+fragment.Duration-0.001 {
				return true
			}
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
	if m.media.touchHls(path) {
		s.markPreparing(index, m.timeline(0).segmentStart(index))
		s.markReady(index)
		return nil
	}
	s.markPreparing(index, m.timeline(0).segmentStart(index))
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
		timeline := m.timeline(s.mediaInfo.Duration)
		total = timeline.segmentCount()
		if !timeline.containsSegment(index) {
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
	// Subtitle extraction must never delay the foreground video horizon. Produce
	// only the requested cue segment here; wider subtitle preparation is an
	// explicitly preemptible background task.
	begin, end := index, index+1
	job, jobCtx, created, err := s.acquireJobLocked(subtitleSegmentJob, index, begin, end, true, m.scheduler, m.settings.MaxJobsPerStream())
	if err != nil {
		s.mtx.Unlock()
		s.markSegmentError(index, err)
		return fmt.Errorf("%w: %v", domain.ErrHlsTemporarilyUnavailable, err)
	}
	if created {
		go m.runSubtitleJob(s, jobCtx, job)
	}
	job.waiters++
	s.mtx.Unlock()
	waitErr := waitForGeneratedAsset(ctx, m.media, path, job)
	s.releaseJobWaiter(job, waitErr)
	if waitErr == nil {
		s.markReady(index)
	}
	return waitErr
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
		s.finishJob(subtitleSegmentJob, job)
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
	job.err = m.runBoundedPackager(ctx, s, job, true, func(runCtx context.Context) error {
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
		if index >= job.begin && index < job.end && !jobFinished(job) {
			return job
		}
	}
	return nil
}

func segmentJobID(kind string, begin, end int, started time.Time) string {
	return fmt.Sprintf("%s-%d-%d-%d", kind, begin, end, started.UnixNano())
}

func cancelAbandonedJobsLocked(jobs map[*segmentJob]struct{}, begin, end int) {
	for job := range jobs {
		overlaps := job.begin < end && begin < job.end
		if overlaps || job.waiters > 0 || jobFinished(job) || job.cancel == nil {
			continue
		}
		job.cancel()
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

func (m *manager) runBoundedPackager(ctx context.Context, s *streamInfo, job *segmentJob, needsCPU bool, run func(context.Context) error) error {
	sourceBlockedDeadline := max(10*time.Second, 3*m.settings.HlsSegmentDuration())
	packagerDeadline := sourceBlockedDeadline
	if needsCPU {
		// Compatibility transcoding can legitimately need longer than real time
		// for its first frame on a small VPS. Source underflow still yields at the
		// shorter deadline above.
		packagerDeadline = max(packagerDeadline, time.Minute)
	}
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
						if rangeEnd > rangeStart && missing > 0 && now.Sub(sourceProgress) >= 500*time.Millisecond {
							s.markJobStage(job, domain.HlsStageSourceBlocked)
						}
						lastProgress := outputProgress
						if sourceProgress.After(lastProgress) {
							lastProgress = sourceProgress
						}
						if diagnosticProgress.After(lastProgress) {
							lastProgress = diagnosticProgress
						}
						if rangeEnd > rangeStart && missing > 0 &&
							now.Sub(sourceProgress) >= sourceBlockedDeadline && now.Sub(outputProgress) >= sourceBlockedDeadline {
							stop(errSourceBlocked)
							return
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
		if needsCPU {
			s.markJobStage(job, domain.HlsStageWaitingCPU)
			err = m.scheduler.transcode(ctx, job.background, job.cancel, attempt)
		} else {
			err = attempt()
		}
		if !errors.Is(err, errSourceBlocked) {
			return err
		}

		_, _, _, rangeStart, rangeEnd := s.packagerProgress(job)
		if rangeEnd <= rangeStart {
			return err
		}
		s.markJobStage(job, domain.HlsStageWaitingSource)
		log.Printf("HLS packager yielded CPU while waiting for torrent range: dir=%s job=%s range=[%d,%d)", filepath.Base(s.paths.outDir), job.id, rangeStart, rangeEnd)
		if waitErr := s.source.WaitRange(ctx, rangeStart, rangeEnd-rangeStart); waitErr != nil {
			return waitErr
		}
	}
}

func waitForGeneratedAsset(ctx context.Context, cache *mediaCache, path string, job *segmentJob) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if cache.touchHls(path) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-job.done:
			if cache.touchHls(path) {
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
