package torrent

import (
	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

type streamKey struct {
	InfoHash string
	Index    int
	Audio    int
	Subtitle int
}

type streamInfo struct {
	cancel                context.CancelFunc
	torrent               *torrent.Torrent
	file                  *torrent.File
	lastView              time.Time
	mtx                   sync.Mutex
	selection             ffmpeg.StreamSelection
	paths                 streamPaths
	source                *torrentSource
	ctx                   context.Context
	ready                 chan struct{}
	readyErr              error
	fatalErr              error
	mediaInfo             domain.MediaInfo
	status                domain.HlsStatus
	statusSegment         int
	segmentErrors         map[int]segmentFailure
	lastTorrentBytes      int64
	progressiveAdvertised int
	progressiveEnded      bool
	progressiveLast       float64
	videoJobs             map[*segmentJob]struct{}
	subtitleJobs          map[*segmentJob]struct{}
	playlistMtx           sync.Mutex
	closing               bool
}

func (s *streamInfo) recordSourceBytes(n int64) {
	if n <= 0 {
		return
	}
	s.mtx.Lock()
	if s.status.Phase == "probing" || s.status.Phase == "preparing" {
		s.status.BytesRead += n
		s.status.LastProgress = time.Now()
	}
	s.mtx.Unlock()
}

func (s *streamInfo) markPreparing(index int, segmentDuration time.Duration) {
	now := time.Now()
	s.mtx.Lock()
	if s.closing || s.fatalErr != nil {
		s.mtx.Unlock()
		return
	}
	if s.status.Phase != "preparing" || s.statusSegment != index {
		s.status = domain.HlsStatus{
			Phase:         "preparing",
			TargetSeconds: float64(index) * segmentDuration.Seconds(),
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
		s.status.Phase = "ready"
		s.status.Message = ""
		s.status.LastProgress = time.Now()
	}
	s.mtx.Unlock()
}

func (s *streamInfo) markSegmentProgress(index int) {
	s.mtx.Lock()
	delete(s.segmentErrors, index)
	if s.status.Phase == "probing" || s.status.Phase == "preparing" {
		s.status.LastProgress = time.Now()
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
		s.status.Phase = "error"
		s.status.Message = publicStreamError(err)
		s.status.LastProgress = time.Now()
	}
	s.mtx.Unlock()
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
	s.status.Phase = "waiting"
	s.status.Message = ""
	s.status.LastProgress = time.Now()
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
	case strings.Contains(message, "reserve") && strings.Contains(message, "hls cache"):
		return "The configured HLS cache is too small for a transcoding window"
	case strings.Contains(message, "ffmpeg"):
		return "FFmpeg could not decode or transcode this media window; check the server log for details"
	case strings.Contains(message, "torrent") || strings.Contains(message, "reader panic"):
		return "The torrent source failed while reading the requested pieces"
	default:
		return "The server could not prepare the requested media window"
	}
}

type segmentJob struct {
	begin      int
	end        int
	cancel     context.CancelFunc
	done       chan struct{}
	startedAt  time.Time
	waiters    int
	background bool
	err        error
	result     ffmpeg.VideoWindowResult
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
	return fmt.Sprintf("%s_%d_a%d_s%d", k.InfoHash, k.Index, k.Audio, k.Subtitle)
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
	if len(parts) != 4 {
		return streamKey{}, fmt.Errorf("bad stream dir")
	}
	if !strings.HasPrefix(parts[2], "a") || !strings.HasPrefix(parts[3], "s") {
		return streamKey{}, fmt.Errorf("bad stream dir")
	}
	audio := strings.TrimPrefix(parts[2], "a")
	subtitle := strings.TrimPrefix(parts[3], "s")
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
	return streamKey{
		InfoHash: parts[0],
		Index:    idx,
		Audio:    audioIdx,
		Subtitle: subIdx,
	}, nil
}
