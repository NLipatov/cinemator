package torrent

import (
	"cinemator/domain"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cinemator/infrastructure/ffmpeg"
)

const (
	streamInitializationTimeout = 10 * time.Minute
	windowGenerationTimeout     = 10 * time.Minute
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
	info, err := s.source.Probe(initCtx)
	if err == nil {
		if info.VideoCodec == "" {
			err = errors.New("selected file has no video stream")
		}
	}
	if err == nil {
		info.Seekable = info.Duration > 0
	}
	if err == nil {
		err = resetStreamOutput(s.paths)
	}
	if err == nil {
		err = ffmpeg.PrepareOnDemandHLS(
			s.paths.outDir,
			s.paths.videoPlaylist,
			s.paths.subtitlePlaylist,
			s.paths.masterPlaylist,
			info,
			s.selection,
			m.settings.HlsSegmentDuration(),
		)
	}

	s.mtx.Lock()
	s.mediaInfo = info
	s.readyErr = err
	if err != nil {
		s.fatalErr = err
		s.status.Phase = "error"
		if s.status.Message == "" {
			s.status.Message = publicStreamError(err)
		}
	} else {
		s.status.Phase = "waiting"
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
	if info.Duration <= 0 {
		m.prefetchProgressiveWindow(s, -1)
	}
}

func (m *manager) EnsureHlsAsset(ctx context.Context, streamDir, assetName string) error {
	key, err := parseStreamDir(streamDir)
	if err != nil {
		return fmt.Errorf("bad stream: %w", err)
	}
	m.mu.Lock()
	s, ok := m.active[key]
	m.mu.Unlock()
	if !ok {
		return errors.New("stream not found")
	}
	if err := s.waitReady(ctx); err != nil {
		return err
	}

	s.mtx.Lock()
	s.lastView = time.Now()
	s.mtx.Unlock()
	if index, ok := parseSegmentName(assetName, "chunk_", ".ts"); ok {
		return m.ensureVideoSegment(ctx, s, index)
	}
	if index, ok := parseSegmentName(assetName, "subs_", ".vtt"); ok {
		return m.ensureSubtitleSegment(ctx, s, index)
	}
	return errors.New("unsupported HLS asset")
}

func (m *manager) ensureVideoSegment(ctx context.Context, s *streamInfo, index int) error {
	s.mtx.Lock()
	closing := s.closing
	fatalErr := s.fatalErr
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
	job := findSegmentJob(s.videoJobs, index)
	if job == nil {
		begin := index
		end := begin + m.settings.HlsWindowSegments()
		if total > 0 {
			end = min(total, end)
		} else {
			end = min(s.progressiveAdvertised, end)
		}
		jobCtx, cancel := context.WithCancel(s.ctx)
		job = &segmentJob{begin: begin, end: end, cancel: cancel, done: make(chan struct{}), startedAt: time.Now()}
		if s.videoJobs == nil {
			s.videoJobs = make(map[*segmentJob]struct{})
		}
		s.videoJobs[job] = struct{}{}
		go m.runVideoJob(s, jobCtx, job)
	}
	job.waiters++
	s.mtx.Unlock()
	err := waitForGeneratedAsset(ctx, path, job)
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
	if s.closing || s.mediaInfo.Duration > 0 || s.progressiveEnded ||
		(requested >= 0 && requested < max(0, s.progressiveAdvertised-window)) {
		s.mtx.Unlock()
		return
	}
	begin := s.progressiveAdvertised
	if findSegmentJob(s.videoJobs, begin) != nil {
		s.mtx.Unlock()
		return
	}
	jobCtx, cancel := context.WithCancel(s.ctx)
	job := &segmentJob{begin: begin, end: begin + window, cancel: cancel, done: make(chan struct{}), startedAt: time.Now(), background: true}
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
	}()
	ctx, cancel := context.WithTimeout(ctx, windowGenerationTimeout)
	defer cancel()
	s.mtx.Lock()
	info := s.mediaInfo
	selection := s.selection
	s.mtx.Unlock()
	job.err = m.withTranscodeSlot(ctx, func() error {
		log.Printf("Generating HLS video window: dir=%s, segments=[%d,%d)", filepath.Base(s.paths.outDir), job.begin, job.end)
		release, err := m.reserveHlsGeneration(job.end - job.begin)
		if err != nil {
			return err
		}
		defer release()
		job.result, job.err = ffmpeg.GenerateVideoWindow(
			ctx,
			s.source.URL(),
			s.paths.outDir,
			info,
			selection,
			job.begin,
			job.end-job.begin,
			m.settings.HlsSegmentDuration(),
			func(index int) {
				s.markSegmentProgress(index)
				if info.Duration <= 0 {
					if err := m.publishProgressiveSegment(s, index); err != nil {
						log.Printf("Update progressive HLS after segment %d: %v", index, err)
					}
				}
			},
		)
		return job.err
	})
	if job.err == nil && info.Duration <= 0 {
		job.err = m.advanceProgressivePlaylist(s, job)
	} else if job.err == nil && job.result.ReachedEnd {
		job.err = m.reconcileKnownDuration(s, job)
	}
	if job.err != nil && !errors.Is(job.err, context.Canceled) {
		log.Printf("Generate HLS video window failed: dir=%s segments=[%d,%d): %v", filepath.Base(s.paths.outDir), job.begin, job.end, job.err)
	}
}

func (m *manager) reconcileKnownDuration(s *streamInfo, job *segmentJob) error {
	actual := float64(job.begin) * m.settings.HlsSegmentDuration().Seconds()
	for _, duration := range job.result.Durations {
		actual += duration
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
	withSubtitles := ffmpeg.UsesTextSubtitles(s.mediaInfo, s.selection)
	s.mtx.Unlock()

	s.playlistMtx.Lock()
	defer s.playlistMtx.Unlock()
	return ffmpeg.UpdateOnDemandHLS(
		s.paths.videoPlaylist,
		s.paths.subtitlePlaylist,
		withSubtitles,
		actual,
		m.settings.HlsSegmentDuration(),
	)
}

func (m *manager) publishProgressiveSegment(s *streamInfo, index int) error {
	s.mtx.Lock()
	if s.progressiveEnded || index+1 <= s.progressiveAdvertised {
		s.mtx.Unlock()
		return nil
	}
	s.progressiveAdvertised = index + 1
	count := s.progressiveAdvertised
	withSubtitles := ffmpeg.UsesTextSubtitles(s.mediaInfo, s.selection)
	s.mtx.Unlock()

	s.playlistMtx.Lock()
	defer s.playlistMtx.Unlock()
	return ffmpeg.UpdateProgressiveHLS(
		s.paths.videoPlaylist,
		s.paths.subtitlePlaylist,
		withSubtitles,
		m.settings.HlsSegmentDuration(),
		count,
		0,
		false,
	)
}

func (m *manager) advanceProgressivePlaylist(s *streamInfo, job *segmentJob) error {
	generatedEnd := job.begin + job.result.Generated
	s.mtx.Lock()
	if job.result.ReachedEnd {
		s.progressiveEnded = true
		s.progressiveAdvertised = max(s.progressiveAdvertised, generatedEnd)
		if len(job.result.Durations) > 0 {
			s.progressiveLast = job.result.Durations[len(job.result.Durations)-1]
		}
	} else {
		s.progressiveAdvertised = max(s.progressiveAdvertised, generatedEnd)
	}
	count := s.progressiveAdvertised
	last := s.progressiveLast
	ended := s.progressiveEnded
	withSubtitles := ffmpeg.UsesTextSubtitles(s.mediaInfo, s.selection)
	s.mtx.Unlock()
	s.playlistMtx.Lock()
	defer s.playlistMtx.Unlock()
	return ffmpeg.UpdateProgressiveHLS(
		s.paths.videoPlaylist,
		s.paths.subtitlePlaylist,
		withSubtitles,
		m.settings.HlsSegmentDuration(),
		count,
		last,
		ended,
	)
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
		return domain.HlsStatus{}, fmt.Errorf("bad stream: %w", err)
	}
	m.mu.Lock()
	s, ok := m.active[key]
	m.mu.Unlock()
	if !ok {
		return domain.HlsStatus{}, errors.New("stream not found")
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
		if s.status.Phase == "probing" || s.status.Phase == "preparing" {
			s.status.LastProgress = now
		}
	}
	status := s.status
	initialized := channelClosed(s.ready)
	videoJobActive := false
	subtitleJobActive := false
	withSubtitles := false
	if targetIndex >= 0 && initialized {
		status.TargetSeconds = targetSeconds
		status.Seekable = s.mediaInfo.Seekable
		status.Duration = s.mediaInfo.Duration
		status.Message = ""
		withSubtitles = ffmpeg.UsesTextSubtitles(s.mediaInfo, s.selection)
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
				if status.LastProgress.Before(job.startedAt) {
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
		_, videoErr := os.Stat(filepath.Join(s.paths.outDir, videoSegmentName(targetIndex)))
		_, subtitleErr := os.Stat(filepath.Join(s.paths.outDir, subtitleSegmentName(targetIndex)))
		videoReady := videoErr == nil
		subtitleReady := !withSubtitles || subtitleErr == nil
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
			status.Message = "Connected peers or the transcoder have not produced data recently"
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
		end := min(total, index+m.settings.HlsWindowSegments())
		jobCtx, cancel := context.WithCancel(s.ctx)
		job = &segmentJob{begin: index, end: end, cancel: cancel, done: make(chan struct{}), startedAt: time.Now()}
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
	}()
	ctx, cancel := context.WithTimeout(ctx, windowGenerationTimeout)
	defer cancel()
	job.err = m.withTranscodeSlot(ctx, func() error {
		for index := job.begin; index < job.end; index++ {
			path := filepath.Join(s.paths.outDir, subtitleSegmentName(index))
			if touchExisting(path) {
				continue
			}
			if err := ffmpeg.GenerateSubtitleSegment(
				ctx,
				s.source.URL(),
				path,
				s.selection.SubtitleTrackIndex,
				index,
				m.settings.HlsSegmentDuration(),
			); err != nil {
				return err
			}
			s.markSegmentProgress(index)
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

func (m *manager) releaseJobWaiter(s *streamInfo, job *segmentJob, waitErr error) {
	s.mtx.Lock()
	if job.waiters > 0 {
		job.waiters--
	}
	if waitErr != nil && job.waiters == 0 && !job.background && !jobFinished(job) {
		job.cancel()
	}
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

func jobFinished(job *segmentJob) bool {
	select {
	case <-job.done:
		return true
	default:
		return false
	}
}

func touchExisting(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
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
