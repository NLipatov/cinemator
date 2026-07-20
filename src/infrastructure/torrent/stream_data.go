package torrent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
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
func (s *streamInfo) reserveJobLocked(scheduler *segmentScheduler, maximum int) (func(), error) {
	if s.closing {
		return nil, context.Canceled
	}
	if len(s.videoJobs)+len(s.subtitleJobs) >= maximum {
		return nil, errStreamJobLimit
	}
	return scheduler.reserveJob()
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

func (s *streamInfo) recordSourceBytes(jobID string, n int64) {
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
				job.lastProgress = now
				statusProgress = s.statusSegment >= job.begin && s.statusSegment < job.end
				break
			}
		}
		for job := range s.subtitleJobs {
			if job.id == jobID {
				job.bytesRead += n
				job.lastProgress = now
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
	}
	if s.status.Phase == domain.HlsPhasePreparing && job != nil {
		if _, videoJob := s.videoJobs[job]; videoJob && s.statusSegment >= job.begin && s.statusSegment < job.end {
			s.status.LastProgress = now
		}
	}
	s.mtx.Unlock()
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
	s.status.Message = ""
	s.status.LastProgress = time.Now()
}

func (s *streamInfo) mediaDuration() float64 {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.mediaInfo.Duration
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
			status.Message = publicStreamError(s.fatalErr)
		} else if failure, ok := s.segmentErrors[targetIndex]; ok {
			status.Phase = domain.HlsPhaseError
			status.Message = failure.message
			status.StartedAt = failure.at
			status.LastProgress = failure.at
		} else if videoJob := findSegmentJob(s.videoJobs, targetIndex); videoJob != nil {
			videoJobActive = true
			status.Phase = domain.HlsPhasePreparing
			status.StartedAt = videoJob.startedAt
			status.LastProgress = videoJob.lastProgress
			status.BytesRead = videoJob.bytesRead
			if status.LastProgress.IsZero() {
				status.LastProgress = videoJob.startedAt
			}
		} else {
			status.Phase = domain.HlsPhaseWaiting
			status.StartedAt = now
			status.LastProgress = now
		}
	}
	s.mtx.Unlock()

	if targetIndex >= 0 && initialized && status.Phase != domain.HlsPhaseError {
		if publishedReady {
			status.Phase = domain.HlsPhaseReady
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
	begin         int
	end           int
	id            string
	cancel        context.CancelFunc
	done          chan struct{}
	startedAt     time.Time
	lastProgress  time.Time
	bytesRead     int64
	waiters       int
	background    bool
	started       bool
	err           error
	result        ffmpeg.VideoWindowResult
	fragments     []ffmpeg.HLSFragment
	directEnd     bool
	releaseSlot   func()
	followEnd     int
	targetSeconds float64
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
	releaseSlot, err := s.reserveJobLocked(scheduler, maximum)
	if err != nil {
		return nil, nil, false, err
	}
	jobCtx, cancel := context.WithCancel(s.ctx)
	now := time.Now()
	job := &segmentJob{
		begin:        begin,
		end:          end,
		id:           segmentJobID(name, begin, end, now),
		cancel:       cancel,
		done:         make(chan struct{}),
		startedAt:    now,
		lastProgress: now,
		background:   background,
		releaseSlot:  releaseSlot,
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
