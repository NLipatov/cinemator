package torrent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"

	"github.com/anacrolix/torrent"
)

type streamKey struct {
	InfoHash  string
	Session   string
	Index     int
	Audio     int
	Subtitle  int
	Transcode bool
}

type mediaKey struct {
	InfoHash string
	Index    int
}

type streamInfo struct {
	cancel                        context.CancelFunc
	torrent                       *torrent.Torrent
	file                          *torrent.File
	lastView                      time.Time
	mtx                           sync.Mutex
	selection                     ffmpeg.StreamSelection
	paths                         streamPaths
	assetVersion                  string
	source                        *torrentSource
	ctx                           context.Context
	ready                         chan struct{}
	readyErr                      error
	fatalErr                      error
	mediaInfo                     domain.MediaInfo
	mediaInfoReady                bool
	mediaKey                      mediaKey
	durationRefinementStarted     bool
	presentationTarget            float64
	directPlay                    bool
	materializedWindows           map[int][]ffmpeg.HLSFragment
	materializedBytes             map[int]int64
	playlistTargetDuration        time.Duration
	publishedFragments            []ffmpeg.HLSFragment
	playlistAnchor                int
	retainedAssets                map[string]time.Time
	status                        domain.HlsStatus
	statusSegment                 int
	segmentErrors                 map[int]segmentFailure
	materializedEnd               int
	playheadSegment               int
	videoDeliverySegment          int
	subtitleDeliverySegment       int
	urgentAhead                   time.Duration
	deliveryRatio                 float64
	deliveryJitter                float64
	deliverySamples               int
	progressiveSubtitles          int
	playlistSequence              int
	playlistDiscontinuitySequence int
	sourceEnded                   bool
	progressiveLast               float64
	progressiveRetry              bool
	videoJobs                     map[*segmentJob]struct{}
	subtitleJobs                  map[*segmentJob]struct{}
	mediaExecutionOnce            sync.Once
	mediaExecution                *priorityWorkerPool
	playlistMtx                   sync.RWMutex
	generationMtx                 sync.RWMutex
	cleanupDone                   chan struct{}
	closing                       bool
}

func (s *streamInfo) reserveMediaExecution(
	ctx context.Context,
	background bool,
	cancel context.CancelFunc,
) (func(), error) {
	s.mediaExecutionOnce.Do(func() {
		s.mediaExecution = newPriorityWorkerPool(1)
	})
	admission, err := s.mediaExecution.acquire(ctx, background, cancel)
	if err != nil {
		return nil, err
	}
	return func() { s.mediaExecution.release(admission) }, nil
}

type streamCacheSnapshot struct {
	paths                streamPaths
	videoJobs            []segmentJobCacheSnapshot
	subtitleJobs         []segmentJobCacheSnapshot
	materializedWindows  map[int][]ffmpeg.HLSFragment
	publishedFragments   []ffmpeg.HLSFragment
	retainedAssets       map[string]time.Time
	progressiveSubtitles int
}

type segmentJobCacheSnapshot struct {
	begin           int
	end             int
	publishedAssets []string
}

type segmentJobStatusSnapshot struct {
	job        *segmentJob
	pieces     map[int]bool
	rangeStart int64
	rangeEnd   int64
}

func (s *streamInfo) touch(now time.Time) {
	s.mtx.Lock()
	s.lastView = now
	s.mtx.Unlock()
}

func (s *streamInfo) idleAt(now time.Time, timeout time.Duration) bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return now.Sub(s.lastView) > timeout
}

func (s *streamInfo) beginClose() (<-chan struct{}, bool) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.closing {
		return s.cleanupDone, false
	}
	if s.cleanupDone == nil {
		s.cleanupDone = make(chan struct{})
	}
	s.closing = true
	if s.cancel != nil {
		s.cancel()
	}
	return s.cleanupDone, true
}

func (s *streamInfo) availabilityErrorLocked() error {
	if s.closing {
		return context.Canceled
	}
	return s.fatalErr
}

func (s *streamInfo) activeJobs() []*segmentJob {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	jobs := make([]*segmentJob, 0, len(s.videoJobs)+len(s.subtitleJobs))
	for job := range s.videoJobs {
		jobs = append(jobs, job)
	}
	for job := range s.subtitleJobs {
		jobs = append(jobs, job)
	}
	return jobs
}

func (s *streamInfo) cacheSnapshot(now time.Time) streamCacheSnapshot {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	snapshot := streamCacheSnapshot{
		paths:                s.paths,
		videoJobs:            make([]segmentJobCacheSnapshot, 0, len(s.videoJobs)),
		subtitleJobs:         make([]segmentJobCacheSnapshot, 0, len(s.subtitleJobs)),
		materializedWindows:  make(map[int][]ffmpeg.HLSFragment, len(s.materializedWindows)),
		publishedFragments:   append([]ffmpeg.HLSFragment(nil), s.publishedFragments...),
		retainedAssets:       make(map[string]time.Time, len(s.retainedAssets)),
		progressiveSubtitles: s.progressiveSubtitles,
	}
	for job := range s.videoJobs {
		if jobSnapshot, ok := snapshotSegmentJob(job); ok {
			snapshot.videoJobs = append(snapshot.videoJobs, jobSnapshot)
		}
	}
	for job := range s.subtitleJobs {
		if jobSnapshot, ok := snapshotSegmentJob(job); ok {
			snapshot.subtitleJobs = append(snapshot.subtitleJobs, jobSnapshot)
		}
	}
	for owner, fragments := range s.materializedWindows {
		snapshot.materializedWindows[owner] = append([]ffmpeg.HLSFragment(nil), fragments...)
	}
	for name, deadline := range s.retainedAssets {
		if deadline.After(now) {
			snapshot.retainedAssets[name] = deadline
		}
	}
	return snapshot
}

func snapshotSegmentJob(job *segmentJob) (segmentJobCacheSnapshot, bool) {
	if job == nil || jobFinished(job) {
		return segmentJobCacheSnapshot{}, false
	}
	snapshot := segmentJobCacheSnapshot{
		begin:           job.begin,
		end:             job.end,
		publishedAssets: make([]string, 0, len(job.publishedAssets)),
	}
	for asset := range job.publishedAssets {
		snapshot.publishedAssets = append(snapshot.publishedAssets, asset)
	}
	return snapshot, true
}

// reserveJobLocked is the single per-session admission point. The scheduler
// contributes only the process-wide capacity token; the session decides
// whether another job belongs to its lifecycle.
func (s *streamInfo) reserveJobLocked(scheduler *segmentScheduler, maximum int, background bool, cancel context.CancelFunc) (func(), error) {
	if s.closing {
		return nil, context.Canceled
	}
	// Foreground playback demand must not lose to speculative work owned by the
	// same stream. Retire lazy subtitles first, then video prefetch, before
	// applying the per-stream limit. The process-wide scheduler applies the same
	// policy across streams.
	for len(s.videoJobs)+len(s.subtitleJobs) >= maximum && !background {
		if retireFirstBackgroundJobLocked(s.subtitleJobs) || retireFirstBackgroundJobLocked(s.videoJobs) {
			continue
		}
		break
	}
	if len(s.videoJobs)+len(s.subtitleJobs) >= maximum {
		return nil, errStreamJobLimit
	}
	return scheduler.reserveJob(background, cancel)
}

func retireFirstBackgroundJobLocked(jobs map[*segmentJob]struct{}) bool {
	for job := range jobs {
		if !job.background || jobFinished(job) {
			continue
		}
		retireSegmentJobLocked(jobs, job)
		return true
	}
	return false
}

func retireSegmentJobLocked(jobs map[*segmentJob]struct{}, job *segmentJob) {
	if job == nil {
		return
	}
	if _, exists := jobs[job]; !exists {
		return
	}
	delete(jobs, job)
	if job.cancel != nil {
		job.cancel()
	}
	job.releaseAdmission()
}

func (s *streamInfo) currentAssetVersion() string {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.assetVersion
}

func findSegmentJobByID(jobID string, jobSets ...map[*segmentJob]struct{}) *segmentJob {
	for _, jobs := range jobSets {
		for job := range jobs {
			if job.id == jobID {
				return job
			}
		}
	}
	return nil
}

func (s *streamInfo) segmentJobActiveLocked(job *segmentJob) bool {
	if job == nil {
		return false
	}
	if _, active := s.videoJobs[job]; active {
		return true
	}
	_, active := s.subtitleJobs[job]
	return active
}

func (s *streamInfo) recordSourceRange(jobID string, offset, length int64) {
	if jobID == "" || length <= 0 || s.source == nil {
		return
	}
	windows := s.source.rangeWindows(offset, length)
	now := time.Now()
	s.mtx.Lock()
	defer s.mtx.Unlock()
	job := findSegmentJobByID(jobID, s.videoJobs, s.subtitleJobs)
	if job == nil {
		return
	}
	changedRange := job.requestedRangeStart != offset || job.requestedRangeEnd != offset+length
	job.requestedRangeStart = offset
	job.requestedRangeEnd = offset + length
	if changedRange {
		job.lastRangeAt = now
		job.lastProgress = now
	}
	if job.requestedPieces == nil {
		job.requestedPieces = make(map[int]bool)
	}
	for _, window := range windows {
		if _, exists := job.requestedPieces[window.index]; !exists {
			job.requestedPieces[window.index] = window.state.Ok && window.state.Complete
		}
	}
}

func (s *streamInfo) recordSourceBytes(jobID string, offset, n int64) {
	if n <= 0 {
		return
	}
	s.mtx.Lock()
	now := time.Now()
	statusProgress := s.status.Phase == domain.HlsPhaseProbing && jobID == ""
	if jobID != "" {
		if job := findSegmentJobByID(jobID, s.videoJobs, s.subtitleJobs); job != nil {
			job.bytesRead += n
			var unique int64
			job.sourceRanges, unique = addSourceRange(job.sourceRanges, offset, offset+n)
			if unique > 0 {
				job.lastSourceProgress = now
				job.lastProgress = now
				job.stage = domain.HlsStagePackaging
			}
			if _, videoJob := s.videoJobs[job]; videoJob {
				statusProgress = s.statusSegment >= job.begin && s.statusSegment < job.end
			}
		}
	}
	if statusProgress && (s.status.Phase == domain.HlsPhaseProbing || s.status.Phase == domain.HlsPhasePreparing) {
		s.status.BytesRead += n
		s.status.LastProgress = now
	}
	s.mtx.Unlock()
}

func (s *streamInfo) markPreparing(index int, targetSeconds float64) {
	now := time.Now()
	s.mtx.Lock()
	if s.closing || s.fatalErr != nil {
		s.mtx.Unlock()
		return
	}
	if s.status.Phase != domain.HlsPhasePreparing || s.statusSegment != index {
		s.status = domain.HlsStatus{
			Phase:         domain.HlsPhasePreparing,
			Stage:         domain.HlsStageQueued,
			Mode:          s.status.Mode,
			TargetSeconds: targetSeconds,
			StartedAt:     now,
			LastProgress:  now,
			Seekable:      s.mediaInfo.Seekable,
			Duration:      s.mediaInfo.Duration,
		}
		s.statusSegment = index
	}
	delete(s.segmentErrors, index)
	s.mtx.Unlock()
}

func (s *streamInfo) markReady(index int) {
	s.mtx.Lock()
	if s.statusSegment == index && s.fatalErr == nil && !s.closing {
		s.status.Phase = domain.HlsPhaseReady
		s.status.Stage = domain.HlsStageReady
		s.status.Message = ""
		s.status.LastProgress = time.Now()
	}
	s.mtx.Unlock()
}

func (s *streamInfo) markSegmentProgress(job *segmentJob, index int) {
	s.mtx.Lock()
	delete(s.segmentErrors, index)
	now := time.Now()
	if job != nil {
		job.lastProgress = now
		job.lastOutputProgress = now
	}
	if s.status.Phase == domain.HlsPhasePreparing && job != nil {
		if _, videoJob := s.videoJobs[job]; videoJob && s.statusSegment >= job.begin && s.statusSegment < job.end {
			s.status.LastProgress = now
		}
	}
	s.mtx.Unlock()
	job.notifyProgress()
}

func (s *streamInfo) markPublished(job *segmentJob, asset string, size int64) {
	if job == nil || size < 0 {
		return
	}
	s.mtx.Lock()
	if job.publishedAssets == nil {
		job.publishedAssets = make(map[string]struct{})
	}
	if _, exists := job.publishedAssets[asset]; !exists {
		job.publishedAssets[asset] = struct{}{}
		job.publishedBytes += size
	}
	now := time.Now()
	job.lastOutputProgress = now
	job.lastProgress = now
	s.mtx.Unlock()
	job.notifyProgress()
}

func (s *streamInfo) markJobStage(job *segmentJob, stage domain.HlsStage) {
	if job == nil {
		return
	}
	s.mtx.Lock()
	if job.stage == stage {
		s.mtx.Unlock()
		return
	}
	job.stage = stage
	now := time.Now()
	job.lastProgress = now
	if stage == domain.HlsStagePackaging {
		job.lastSourceProgress = now
		job.lastOutputProgress = now
	}
	_, videoJob := s.videoJobs[job]
	if videoJob && s.statusSegment >= job.begin && s.statusSegment < job.end {
		s.status.Stage = stage
		s.status.LastProgress = now
	}
	s.mtx.Unlock()
}

func (s *streamInfo) packagerProgress(job *segmentJob) (source, output, diagnostic time.Time, rangeStart, rangeEnd int64) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return job.lastSourceProgress, job.lastOutputProgress, job.lastRangeAt, job.requestedRangeStart, job.requestedRangeEnd
}

func (s *streamInfo) markJobError(job *segmentJob, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	s.mtx.Lock()
	_, videoJob := s.videoJobs[job]
	_, subtitleJob := s.subtitleJobs[job]
	if !videoJob && !subtitleJob {
		s.mtx.Unlock()
		return
	}
	job.stage = domain.HlsStageError
	if s.fatalErr == nil && (videoJob || subtitleJob) {
		if s.segmentErrors == nil {
			s.segmentErrors = make(map[int]segmentFailure)
		}
		failure := segmentFailure{message: publicStreamError(err), at: time.Now()}
		for index := job.begin; index < job.end; index++ {
			s.segmentErrors[index] = failure
		}
		for len(s.segmentErrors) > 256 {
			oldestIndex := -1
			var oldest time.Time
			for index, candidate := range s.segmentErrors {
				if oldestIndex < 0 || candidate.at.Before(oldest) {
					oldestIndex = index
					oldest = candidate.at
				}
			}
			delete(s.segmentErrors, oldestIndex)
		}
	}
	if s.fatalErr == nil && (videoJob || subtitleJob) && s.statusSegment >= job.begin && s.statusSegment < job.end {
		s.status.Phase = domain.HlsPhaseError
		s.status.Stage = domain.HlsStageError
		s.status.Message = publicStreamError(err)
		s.status.LastProgress = time.Now()
	}
	s.mtx.Unlock()
}

func (s *streamInfo) markSegmentError(index int, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.closing || s.fatalErr != nil {
		return
	}
	if s.segmentErrors == nil {
		s.segmentErrors = make(map[int]segmentFailure)
	}
	now := time.Now()
	failure := segmentFailure{message: publicStreamError(err), at: now}
	s.segmentErrors[index] = failure
	if s.statusSegment == index {
		s.status.Phase = domain.HlsPhaseError
		s.status.Stage = domain.HlsStageError
		s.status.Message = failure.message
		s.status.LastProgress = now
	}
}

func (s *streamInfo) markJobCanceled(job *segmentJob) {
	if job == nil {
		return
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.fatalErr != nil || s.statusSegment < job.begin || s.statusSegment >= job.end {
		return
	}
	if _, ok := s.videoJobs[job]; !ok {
		return
	}
	for other := range s.videoJobs {
		if other != job && s.statusSegment >= other.begin && s.statusSegment < other.end && !jobFinished(other) {
			return
		}
	}
	s.status.Phase = domain.HlsPhaseWaiting
	s.status.Stage = domain.HlsStageCancelled
	s.status.Message = ""
	s.status.LastProgress = time.Now()
}

func (s *streamInfo) mediaDuration() float64 {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.mediaInfo.Duration
}

func (s *streamInfo) sourceReadahead(jobID string, maximum int64) int64 {
	s.mtx.Lock()
	bitrate := s.mediaInfo.Bitrate
	horizon := 4 * time.Second
	startupCap := int64(16 << 20)
	for job := range s.videoJobs {
		if job.id != jobID {
			continue
		}
		if job.background {
			horizon = 30 * time.Second
			startupCap = 0
		} else if job.publishedBytes > 0 {
			horizon = 15 * time.Second
			startupCap = 0
		}
		break
	}
	s.mtx.Unlock()
	pieceLength := int64(0)
	if s.torrent != nil && s.torrent.Info() != nil {
		pieceLength = s.torrent.Info().PieceLength
	}
	return playbackReadaheadBytes(maximum, pieceLength, bitrate, horizon, startupCap)
}

func (s *streamInfo) recordJobDelivery(job *segmentJob, segmentDuration time.Duration, now time.Time) {
	if job == nil || segmentDuration <= 0 || job.end <= job.begin {
		return
	}
	mediaDuration := time.Duration(job.end-job.begin) * segmentDuration
	sample := min(4.0, max(0.0, now.Sub(job.startedAt).Seconds()/mediaDuration.Seconds()))
	s.mtx.Lock()
	if s.deliverySamples == 0 {
		s.deliveryRatio = sample
	} else {
		s.deliveryJitter = 0.75*s.deliveryJitter + 0.25*math.Abs(sample-s.deliveryRatio)
		s.deliveryRatio = 0.75*s.deliveryRatio + 0.25*sample
	}
	s.deliverySamples++
	risk := s.deliveryRatio + s.deliveryJitter
	if risk >= 0.75 {
		s.urgentAhead = 60 * time.Second
	} else if s.deliverySamples >= 3 && risk <= 0.5 {
		s.urgentAhead = 30 * time.Second
	}
	s.mtx.Unlock()
}

func playbackReadaheadBytes(maximum, pieceLength, bitrate int64, horizon time.Duration, startupCap int64) int64 {
	if maximum <= 0 {
		maximum = 128 << 20
	}
	if bitrate <= 0 {
		bitrate = 8_000_000
	}
	target := max(int64(4<<20), int64(math.Ceil(float64(bitrate)*horizon.Seconds()/8)))
	if startupCap > 0 {
		target = min(target, startupCap)
	}
	target = min(target, maximum)
	if pieceLength > 0 {
		target = min(maximum, max(pieceLength, ((target+pieceLength-1)/pieceLength)*pieceLength))
	}
	return max(int64(1), target)
}

func snapshotJobStatus(status *domain.HlsStatus, job *segmentJob) segmentJobStatusSnapshot {
	status.Stage = job.stage
	status.WorkClass = "foreground"
	if job.background {
		status.WorkClass = "background"
	}
	status.StartedAt = job.startedAt
	status.LastProgress = job.lastProgress
	if status.LastProgress.IsZero() {
		status.LastProgress = job.startedAt
	}
	status.BytesRead = job.bytesRead
	status.PublishedBytes = job.publishedBytes
	status.RequestedRangeStart = job.requestedRangeStart
	status.RequestedRangeEnd = job.requestedRangeEnd

	snapshot := segmentJobStatusSnapshot{
		job:        job,
		rangeStart: job.requestedRangeStart,
		rangeEnd:   job.requestedRangeEnd,
	}
	if len(job.requestedPieces) > 0 {
		snapshot.pieces = make(map[int]bool, len(job.requestedPieces))
		for index, complete := range job.requestedPieces {
			snapshot.pieces[index] = complete
		}
	}
	return snapshot
}

func (s *streamInfo) updateJobSourceRate(job *segmentJob, peerBytes int64, now time.Time) int64 {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if !s.segmentJobActiveLocked(job) {
		return 0
	}
	if job.lastPeerRateAt.IsZero() {
		job.lastPeerRateAt = job.startedAt
	}
	elapsed := now.Sub(job.lastPeerRateAt).Seconds()
	if delta := peerBytes - job.lastPeerBytes; delta > 0 && elapsed > 0 {
		sample := float64(delta*8) / elapsed
		if job.sourceRate == 0 {
			job.sourceRate = sample
		} else {
			job.sourceRate = 0.75*job.sourceRate + 0.25*sample
		}
		job.lastPeerBytes = peerBytes
		job.lastPeerRateAt = now
	}
	return int64(job.sourceRate)
}

// playbackStatus is the session's single read model for phase transitions.
// The manager supplies only external torrent counters and the pure timeline.
func (s *streamInfo) playbackStatus(targetSeconds float64, timeline playbackTimeline, now time.Time, activePeers, totalPeers int, subtitlesReady bool) domain.HlsStatus {
	targetIndex := -1
	hasTarget := targetSeconds >= 0 && !math.IsNaN(targetSeconds) && !math.IsInf(targetSeconds, 0)

	s.playlistMtx.RLock()
	s.mtx.Lock()
	if hasTarget {
		target := timeline.locate(targetSeconds)
		targetSeconds = target.sourceSeconds
		targetIndex = target.segment
	}
	status := s.status
	status.Generation = s.assetVersion
	initialized := channelClosed(s.ready)
	jobActive := false
	publishedReady := false
	var jobSnapshot segmentJobStatusSnapshot
	if targetIndex >= 0 && initialized {
		status.TargetSeconds = targetSeconds
		status.Seekable = s.mediaInfo.Seekable
		status.Duration = s.mediaInfo.Duration
		status.Message = ""
		status.PresentationOriginSeconds = 0
		if len(s.publishedFragments) > 0 {
			// The client offset belongs to the immutable head of the published
			// playlist, not to the wider disk cache. Background backfill may
			// extend that cache backwards without changing the active HLS
			// presentation's time origin.
			status.PresentationOriginSeconds = s.publishedFragments[0].Start
		}
		publishedReady = fragmentsCoverTime(s.publishedFragments, targetSeconds)
		if s.fatalErr != nil {
			status.Phase = domain.HlsPhaseError
			status.Stage = domain.HlsStageError
			status.Message = publicStreamError(s.fatalErr)
		} else if failure, ok := s.segmentErrors[targetIndex]; ok {
			status.Phase = domain.HlsPhaseError
			status.Stage = domain.HlsStageError
			status.Message = failure.message
			status.StartedAt = failure.at
			status.LastProgress = failure.at
		} else if videoJob := findSegmentJob(s.videoJobs, targetIndex); videoJob != nil {
			jobActive = true
			status.Phase = domain.HlsPhasePreparing
			jobSnapshot = snapshotJobStatus(&status, videoJob)
		} else if publishedReady && !subtitlesReady {
			status.Phase = domain.HlsPhasePreparing
			status.Stage = domain.HlsStageQueued
			status.Message = "Preparing selected subtitles"
			status.StartedAt = now
			status.LastProgress = now
			if subtitleJob := findSegmentJob(s.subtitleJobs, targetIndex); subtitleJob != nil {
				jobActive = true
				jobSnapshot = snapshotJobStatus(&status, subtitleJob)
			}
		} else {
			status.Phase = domain.HlsPhaseWaiting
			status.Stage = domain.HlsStageWaitingSource
			status.StartedAt = now
			status.LastProgress = now
		}
	}
	s.mtx.Unlock()
	s.playlistMtx.RUnlock()
	if s.source != nil && len(jobSnapshot.pieces) > 0 {
		status.CacheBytes, status.PeerBytes = s.source.requestedPieceBytes(jobSnapshot.pieces)
		status.MissingPieces, status.RangePieces = s.source.rangePieceCounts(jobSnapshot.rangeStart, jobSnapshot.rangeEnd-jobSnapshot.rangeStart)
		status.SourceRateBitsPerSecond = s.updateJobSourceRate(jobSnapshot.job, status.PeerBytes, now)
	}

	if targetIndex >= 0 && initialized && status.Phase != domain.HlsPhaseError {
		if publishedReady && subtitlesReady {
			status.Phase = domain.HlsPhaseReady
			status.Stage = domain.HlsStageReady
			status.Message = ""
			status.LastProgress = now
		} else if jobActive {
			status.Phase = domain.HlsPhasePreparing
		}
	}
	status.ActivePeers = activePeers
	status.TotalPeers = totalPeers
	return classifyHlsStatus(status, now)
}

func publicStreamError(err error) string {
	if err == nil {
		return "Stream failed"
	}
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "Preparing this media window timed out"
	case errors.Is(err, errPackagerNoOutput):
		return "The media packager stopped making output progress"
	case strings.Contains(message, "no space left"):
		return "The server ran out of disk space while preparing video"
	case strings.Contains(message, "insufficient disk") || strings.Contains(message, "free-space floor") || strings.Contains(message, "free-inode"):
		return "The server is preserving its emergency disk reserve; free space or lower the configured cache budget"
	case strings.Contains(message, "shared cache") && (strings.Contains(message, "reserve") || strings.Contains(message, "limit")):
		return "The configured shared cache is too small for this media window"
	case errors.Is(err, errStreamJobQueueFull), errors.Is(err, errStreamJobLimit), strings.Contains(message, "active stream limit"):
		return "The server is at its configured streaming capacity; retry shortly"
	case strings.Contains(message, "ffmpeg"):
		return "FFmpeg could not decode or transcode this media window; check the server log for details"
	case strings.Contains(message, "torrent") || strings.Contains(message, "reader panic"):
		return "The torrent source failed while reading the requested pieces"
	default:
		return "The server could not prepare the requested media window"
	}
}

type segmentJob struct {
	begin                int
	end                  int
	materializationBegin int
	id                   string
	cancel               context.CancelFunc
	done                 chan struct{}
	progress             chan struct{}
	startedAt            time.Time
	lastProgress         time.Time
	bytesRead            int64
	publishedBytes       int64
	publishedAssets      map[string]struct{}
	lastSourceProgress   time.Time
	lastOutputProgress   time.Time
	lastRangeAt          time.Time
	requestedRangeStart  int64
	requestedRangeEnd    int64
	requestedPieces      map[int]bool
	lastPeerBytes        int64
	lastPeerRateAt       time.Time
	sourceRate           float64
	sourceRanges         []sourceByteRange
	stage                domain.HlsStage
	waiters              int
	background           bool
	started              bool
	err                  error
	result               ffmpeg.VideoWindowResult
	fragments            []ffmpeg.HLSFragment
	directEnd            bool
	releaseSlot          func()
	releaseOnce          sync.Once
	ctx                  context.Context
	targetSeconds        float64
}

type sourceByteRange struct {
	start int64
	end   int64
}

// addSourceRange keeps a compact union and returns only newly observed bytes.
// Re-reading cached bytes therefore cannot keep a stuck packager alive.
func addSourceRange(ranges []sourceByteRange, start, end int64) ([]sourceByteRange, int64) {
	if end <= start {
		return ranges, 0
	}
	before := int64(0)
	for _, current := range ranges {
		before += current.end - current.start
	}
	ranges = append(ranges, sourceByteRange{start: start, end: end})
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	merged := ranges[:0]
	for _, current := range ranges {
		if len(merged) == 0 || current.start > merged[len(merged)-1].end {
			merged = append(merged, current)
			continue
		}
		if current.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = current.end
		}
	}
	after := int64(0)
	for _, current := range merged {
		after += current.end - current.start
	}
	return merged, after - before
}

type segmentJobKind uint8

const (
	videoSegmentJob segmentJobKind = iota
	subtitleSegmentJob
)

func (s *streamInfo) acquireJobLocked(kind segmentJobKind, requestIndex, begin, end int, background bool, scheduler *segmentScheduler, maximum int) (*segmentJob, context.Context, bool, error) {
	jobs := s.videoJobs
	name := "video"
	if kind == subtitleSegmentJob {
		jobs = s.subtitleJobs
		name = "subtitle"
	}
	if job := findSegmentJob(jobs, requestIndex); job != nil {
		if background || !job.background || job.cancel == nil {
			return job, nil, false, nil
		}
		retireSegmentJobLocked(jobs, job)
	}
	jobCtx, cancel := context.WithCancel(s.ctx)
	releaseSlot, err := s.reserveJobLocked(scheduler, maximum, background, cancel)
	if err != nil {
		cancel()
		return nil, nil, false, err
	}
	now := time.Now()
	job := &segmentJob{
		begin:                begin,
		end:                  end,
		materializationBegin: requestIndex,
		id:                   segmentJobID(name, begin, end, now),
		cancel:               cancel,
		done:                 make(chan struct{}),
		progress:             make(chan struct{}, 1),
		startedAt:            now,
		lastProgress:         now,
		lastSourceProgress:   now,
		lastOutputProgress:   now,
		stage:                domain.HlsStageQueued,
		background:           background,
		releaseSlot:          releaseSlot,
		ctx:                  jobCtx,
	}
	if jobs == nil {
		jobs = make(map[*segmentJob]struct{})
		if kind == videoSegmentJob {
			s.videoJobs = jobs
		} else {
			s.subtitleJobs = jobs
		}
	}
	jobs[job] = struct{}{}
	return job, jobCtx, true, nil
}

func (s *streamInfo) completeJob(kind segmentJobKind, job *segmentJob) {
	if errors.Is(job.err, context.Canceled) {
		s.markJobCanceled(job)
	} else {
		s.markJobError(job, job.err)
	}
	s.mtx.Lock()
	if kind == videoSegmentJob {
		delete(s.videoJobs, job)
	} else {
		delete(s.subtitleJobs, job)
	}
	close(job.done)
	s.mtx.Unlock()
	job.releaseAdmission()
}

func (j *segmentJob) notifyProgress() {
	if j == nil || j.progress == nil {
		return
	}
	select {
	case j.progress <- struct{}{}:
	default:
	}
}

func (j *segmentJob) releaseAdmission() {
	if j == nil {
		return
	}
	j.releaseOnce.Do(func() {
		if j.releaseSlot != nil {
			j.releaseSlot()
		}
	})
}

type segmentFailure struct {
	message string
	at      time.Time
}

type streamPaths struct {
	outDir           string
	videoPlaylist    string
	subtitlePlaylist string
	masterPlaylist   string
}

func (k streamKey) dirName() string {
	name := fmt.Sprintf("%s_%d_a%d_s%d_t%d", k.InfoHash, k.Index, k.Audio, k.Subtitle, btoi(k.Transcode))
	if k.Session != "" {
		name += "_p" + k.Session
	}
	return name
}

func (k streamKey) paths(root string) streamPaths {
	outDir := filepath.Join(root, k.dirName())
	return streamPaths{
		outDir:           outDir,
		videoPlaylist:    filepath.Join(outDir, "index.m3u8"),
		subtitlePlaylist: filepath.Join(outDir, "subs.m3u8"),
		masterPlaylist:   filepath.Join(outDir, "master.m3u8"),
	}
}

func parseStreamDir(name string) (streamKey, error) {
	parts := strings.Split(name, "_")
	if len(parts) < 5 || len(parts) > 6 {
		return streamKey{}, fmt.Errorf("bad stream dir")
	}
	if !strings.HasPrefix(parts[2], "a") || !strings.HasPrefix(parts[3], "s") || !strings.HasPrefix(parts[4], "t") {
		return streamKey{}, fmt.Errorf("bad stream dir")
	}
	session := ""
	if len(parts) == 6 {
		if !strings.HasPrefix(parts[5], "p") {
			return streamKey{}, fmt.Errorf("bad stream dir")
		}
		session = strings.TrimPrefix(parts[5], "p")
		if !validPlaybackSession(session) {
			return streamKey{}, fmt.Errorf("bad playback session")
		}
	}
	audio := strings.TrimPrefix(parts[2], "a")
	subtitle := strings.TrimPrefix(parts[3], "s")
	transcode := strings.TrimPrefix(parts[4], "t")
	idx, err := strconv.Atoi(parts[1])
	if err != nil {
		return streamKey{}, err
	}
	audioIdx, err := strconv.Atoi(audio)
	if err != nil {
		return streamKey{}, err
	}
	subIdx, err := strconv.Atoi(subtitle)
	if err != nil {
		return streamKey{}, err
	}
	transcodeValue, err := strconv.Atoi(transcode)
	if err != nil || transcodeValue < 0 || transcodeValue > 1 {
		return streamKey{}, fmt.Errorf("bad transcode mode")
	}
	return streamKey{
		InfoHash:  parts[0],
		Session:   session,
		Index:     idx,
		Audio:     audioIdx,
		Subtitle:  subIdx,
		Transcode: transcodeValue == 1,
	}, nil
}

func validPlaybackSession(session string) bool {
	if len(session) == 0 || len(session) > 64 {
		return false
	}
	for _, char := range session {
		if char >= 'a' && char <= 'z' ||
			char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' ||
			char == '-' {
			continue
		}
		return false
	}
	return true
}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}
