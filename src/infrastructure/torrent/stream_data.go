package torrent

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	cancel                           context.CancelFunc
	torrent                          *torrent.Torrent
	file                             *torrent.File
	lastView                         time.Time
	mtx                              sync.Mutex
	selection                        ffmpeg.StreamSelection
	paths                            streamPaths
	assetVersion                     string
	source                           *torrentSource
	ctx                              context.Context
	ready                            chan struct{}
	readyErr                         error
	fatalErr                         error
	mediaInfo                        domain.MediaInfo
	mediaInfoReady                   bool
	mediaKey                         mediaKey
	durationRefinementStarted        bool
	presentationTarget               float64
	directPlay                       bool
	directWindows                    map[int][]ffmpeg.HLSFragment
	playbackWindow                   int
	materializedTarget               time.Duration
	status                           domain.HlsStatus
	statusSegment                    int
	segmentErrors                    map[int]segmentFailure
	lastTorrentBytes                 int64
	progressiveAdvertised            int
	progressiveDemand                int
	progressiveTarget                time.Duration
	deliveryRatio                    float64
	deliveryJitter                   float64
	deliverySamples                  int
	progressiveSubtitles             int
	progressiveSequence              int
	progressiveDiscontinuitySequence int
	progressiveEnded                 bool
	progressiveLast                  float64
	progressiveRetry                 bool
	videoJobs                        map[*segmentJob]struct{}
	subtitleJobs                     map[*segmentJob]struct{}
	playlistMtx                      sync.RWMutex
	generationMtx                    sync.RWMutex
	cleanupDone                      chan struct{}
	closing                          bool
}

type streamCacheSnapshot struct {
	paths                streamPaths
	publishing           bool
	videoJobs            []*segmentJob
	subtitleJobs         []*segmentJob
	directWindows        map[int][]ffmpeg.HLSFragment
	playbackWindow       int
	progressiveSubtitles int
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

// reserveJobLocked is the single per-session admission point. The scheduler
// contributes only the process-wide capacity token; the session decides
// whether another job belongs to its lifecycle.
func (s *streamInfo) reserveJobLocked(scheduler *segmentScheduler, maximum int, background bool, cancel context.CancelFunc) (func(), error) {
	if s.closing {
		return nil, context.Canceled
	}
	if len(s.videoJobs)+len(s.subtitleJobs) >= maximum {
		return nil, errStreamJobLimit
	}
	return scheduler.reserveJob(background, cancel)
}

func (s *streamInfo) currentAssetVersion() string {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.assetVersion
}

func (s *streamInfo) cacheSnapshot() streamCacheSnapshot {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	snapshot := streamCacheSnapshot{
		paths:                s.paths,
		publishing:           s.ready != nil && !channelClosed(s.ready),
		videoJobs:            make([]*segmentJob, 0, len(s.videoJobs)),
		subtitleJobs:         make([]*segmentJob, 0, len(s.subtitleJobs)),
		directWindows:        make(map[int][]ffmpeg.HLSFragment, len(s.directWindows)),
		playbackWindow:       s.playbackWindow,
		progressiveSubtitles: s.progressiveSubtitles,
	}
	for job := range s.videoJobs {
		snapshot.videoJobs = append(snapshot.videoJobs, job)
	}
	for job := range s.subtitleJobs {
		snapshot.subtitleJobs = append(snapshot.subtitleJobs, job)
	}
	for owner, fragments := range s.directWindows {
		snapshot.directWindows[owner] = append([]ffmpeg.HLSFragment(nil), fragments...)
	}
	return snapshot
}

func (s *streamInfo) recordSourceRange(jobID string, offset, length int64) {
	if jobID == "" || length <= 0 || s.source == nil {
		return
	}
	windows := s.source.rangeWindows(offset, length)
	now := time.Now()
	s.mtx.Lock()
	defer s.mtx.Unlock()
	record := func(jobs map[*segmentJob]struct{}) bool {
		for job := range jobs {
			if job.id != jobID {
				continue
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
			return true
		}
		return false
	}
	if !record(s.videoJobs) {
		record(s.subtitleJobs)
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
		for job := range s.videoJobs {
			if job.id == jobID {
				job.bytesRead += n
				var unique int64
				job.sourceRanges, unique = addSourceRange(job.sourceRanges, offset, offset+n)
				if unique > 0 {
					job.lastSourceProgress = now
					job.lastProgress = now
					job.stage = domain.HlsStagePackaging
				}
				statusProgress = s.statusSegment >= job.begin && s.statusSegment < job.end
				break
			}
		}
		for job := range s.subtitleJobs {
			if job.id == jobID {
				job.bytesRead += n
				var unique int64
				job.sourceRanges, unique = addSourceRange(job.sourceRanges, offset, offset+n)
				if unique > 0 {
					job.lastSourceProgress = now
					job.lastProgress = now
					job.stage = domain.HlsStagePackaging
				}
				break
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
	if s.statusSegment >= job.begin && s.statusSegment < job.end {
		s.status.Stage = stage
		s.status.LastProgress = now
	}
	s.mtx.Unlock()
	workClass := "foreground"
	if job.background {
		workClass = "background"
	}
	log.Printf("HLS job stage: dir=%s job=%s segments=[%d,%d) stage=%s class=%s", filepath.Base(s.paths.outDir), job.id, job.begin, job.end, stage, workClass)
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
	if s.fatalErr == nil && (videoJob || subtitleJob) {
		if s.segmentErrors == nil {
			s.segmentErrors = make(map[int]segmentFailure)
		}
		failure := segmentFailure{message: publicStreamError(err), at: time.Now()}
		job.stage = domain.HlsStageError
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
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.fatalErr != nil || s.statusSegment < job.begin || s.statusSegment >= job.end {
		return
	}
	if _, ok := s.videoJobs[job]; !ok {
		if _, ok := s.subtitleJobs[job]; !ok {
			return
		}
	}
	for other := range s.videoJobs {
		if other != job && s.statusSegment >= other.begin && s.statusSegment < other.end && !jobFinished(other) {
			return
		}
	}
	for other := range s.subtitleJobs {
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
		s.progressiveTarget = 60 * time.Second
	} else if s.deliverySamples >= 3 && risk <= 0.5 {
		s.progressiveTarget = 30 * time.Second
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

// playbackStatus is the session's single read model for phase transitions.
// The manager supplies only external torrent counters and the pure timeline.
func (s *streamInfo) playbackStatus(targetSeconds float64, timeline playbackTimeline, now time.Time, usefulBytes int64, activePeers, totalPeers int) domain.HlsStatus {
	targetIndex := -1
	hasTarget := targetSeconds >= 0 && !math.IsNaN(targetSeconds) && !math.IsInf(targetSeconds, 0)

	s.mtx.Lock()
	if hasTarget {
		target := timeline.locate(targetSeconds)
		targetSeconds = target.sourceSeconds
		targetIndex = target.segment
	}
	if usefulBytes > s.lastTorrentBytes {
		s.lastTorrentBytes = usefulBytes
	}
	status := s.status
	status.Generation = s.assetVersion
	initialized := channelClosed(s.ready)
	videoJobActive := false
	publishedReady := false
	var activeVideoJob *segmentJob
	var requestedPieces map[int]bool
	var requestedRangeStart, requestedRangeEnd int64
	if targetIndex >= 0 && initialized {
		status.TargetSeconds = targetSeconds
		status.Seekable = s.mediaInfo.Seekable
		status.Duration = s.mediaInfo.Duration
		status.Message = ""
		if origin, ok := materializedPresentationOrigin(s.directWindows, s.presentationTarget); ok {
			status.PresentationOriginSeconds = origin
		}
		publishedReady = directFragmentsCoverTime(s.directWindows, targetSeconds)
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
			activeVideoJob = videoJob
			videoJobActive = true
			status.Phase = domain.HlsPhasePreparing
			status.Stage = videoJob.stage
			status.WorkClass = "foreground"
			if videoJob.background {
				status.WorkClass = "background"
			}
			status.StartedAt = videoJob.startedAt
			status.LastProgress = videoJob.lastProgress
			status.BytesRead = videoJob.bytesRead
			status.PublishedBytes = videoJob.publishedBytes
			status.RequestedRangeStart = videoJob.requestedRangeStart
			status.RequestedRangeEnd = videoJob.requestedRangeEnd
			requestedRangeStart = videoJob.requestedRangeStart
			requestedRangeEnd = videoJob.requestedRangeEnd
			if len(videoJob.requestedPieces) > 0 {
				requestedPieces = make(map[int]bool, len(videoJob.requestedPieces))
				for index, complete := range videoJob.requestedPieces {
					requestedPieces[index] = complete
				}
			}
			if status.LastProgress.IsZero() {
				status.LastProgress = videoJob.startedAt
			}
		} else {
			status.Phase = domain.HlsPhaseWaiting
			status.Stage = domain.HlsStageWaitingSource
			status.StartedAt = now
			status.LastProgress = now
		}
	}
	s.mtx.Unlock()
	if s.source != nil && len(requestedPieces) > 0 {
		status.CacheBytes, status.PeerBytes = s.source.requestedPieceBytes(requestedPieces)
		status.MissingPieces, status.RangePieces = s.source.rangePieceCounts(requestedRangeStart, requestedRangeEnd-requestedRangeStart)
		if activeVideoJob != nil {
			s.mtx.Lock()
			if _, active := s.videoJobs[activeVideoJob]; active {
				if activeVideoJob.lastPeerRateAt.IsZero() {
					activeVideoJob.lastPeerRateAt = activeVideoJob.startedAt
				}
				elapsed := now.Sub(activeVideoJob.lastPeerRateAt).Seconds()
				if delta := status.PeerBytes - activeVideoJob.lastPeerBytes; delta > 0 && elapsed > 0 {
					sample := float64(delta*8) / elapsed
					if activeVideoJob.sourceRate == 0 {
						activeVideoJob.sourceRate = sample
					} else {
						activeVideoJob.sourceRate = 0.75*activeVideoJob.sourceRate + 0.25*sample
					}
					activeVideoJob.lastPeerBytes = status.PeerBytes
					activeVideoJob.lastPeerRateAt = now
				}
				status.SourceRateBitsPerSecond = int64(activeVideoJob.sourceRate)
			}
			s.mtx.Unlock()
		}
	}

	if targetIndex >= 0 && initialized && status.Phase != domain.HlsPhaseError {
		if publishedReady {
			status.Phase = domain.HlsPhaseReady
			status.Stage = domain.HlsStageReady
			status.Message = ""
			status.LastProgress = now
		} else if videoJobActive {
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
	case errors.Is(err, errSourceBlocked):
		return "The media packager is waiting for required torrent pieces"
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
	begin               int
	end                 int
	id                  string
	cancel              context.CancelFunc
	done                chan struct{}
	startedAt           time.Time
	lastProgress        time.Time
	bytesRead           int64
	publishedBytes      int64
	publishedAssets     map[string]struct{}
	lastSourceProgress  time.Time
	lastOutputProgress  time.Time
	lastRangeAt         time.Time
	requestedRangeStart int64
	requestedRangeEnd   int64
	requestedPieces     map[int]bool
	lastPeerBytes       int64
	lastPeerRateAt      time.Time
	sourceRate          float64
	sourceRanges        []sourceByteRange
	stage               domain.HlsStage
	waiters             int
	background          bool
	started             bool
	err                 error
	result              ffmpeg.VideoWindowResult
	fragments           []ffmpeg.HLSFragment
	directEnd           bool
	releaseSlot         func()
	targetSeconds       float64
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
		if kind != videoSegmentJob || background || !job.background || job.cancel == nil {
			return job, nil, false, nil
		}
		job.cancel()
	}
	if kind == videoSegmentJob && !background {
		cancelAbandonedJobsLocked(jobs, begin, end)
	}
	jobCtx, cancel := context.WithCancel(s.ctx)
	releaseSlot, err := s.reserveJobLocked(scheduler, maximum, background, cancel)
	if err != nil {
		cancel()
		return nil, nil, false, err
	}
	now := time.Now()
	job := &segmentJob{
		begin:              begin,
		end:                end,
		id:                 segmentJobID(name, begin, end, now),
		cancel:             cancel,
		done:               make(chan struct{}),
		startedAt:          now,
		lastProgress:       now,
		lastSourceProgress: now,
		lastOutputProgress: now,
		stage:              domain.HlsStageQueued,
		background:         background,
		releaseSlot:        releaseSlot,
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

func (s *streamInfo) finishJob(kind segmentJobKind, job *segmentJob) {
	s.mtx.Lock()
	if kind == videoSegmentJob {
		delete(s.videoJobs, job)
	} else {
		delete(s.subtitleJobs, job)
	}
	s.mtx.Unlock()
	job.releaseAdmission()
}

func (j *segmentJob) releaseAdmission() {
	if j != nil && j.releaseSlot != nil {
		j.releaseSlot()
		j.releaseSlot = nil
	}
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
	return fmt.Sprintf("%s_%d_a%d_s%d_t%d", k.InfoHash, k.Index, k.Audio, k.Subtitle, btoi(k.Transcode))
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
	if len(parts) != 5 {
		return streamKey{}, fmt.Errorf("bad stream dir")
	}
	if !strings.HasPrefix(parts[2], "a") || !strings.HasPrefix(parts[3], "s") || !strings.HasPrefix(parts[4], "t") {
		return streamKey{}, fmt.Errorf("bad stream dir")
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
		Index:     idx,
		Audio:     audioIdx,
		Subtitle:  subIdx,
		Transcode: transcodeValue == 1,
	}, nil
}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}
