package torrent

import (
	"math"
	"time"
)

// playbackTimeline is the pure mapping between source time and canonical HLS
// work units. It deliberately knows nothing about playlists, files, torrents,
// or FFmpeg.
type playbackTimeline struct {
	segmentDuration time.Duration
	windowSegments  int
	durationSeconds float64
}

type playbackTarget struct {
	sourceSeconds       float64
	segment             int
	windowBegin         int
	windowEnd           int
	windowOriginSeconds float64
}

type playbackInterval struct {
	startSeconds    float64
	durationSeconds float64
}

func newPlaybackTimeline(segmentDuration time.Duration, windowSegments int, durationSeconds float64) playbackTimeline {
	if segmentDuration <= 0 {
		segmentDuration = time.Second
	}
	if windowSegments <= 0 {
		windowSegments = 1
	}
	if durationSeconds < 0 || math.IsNaN(durationSeconds) || math.IsInf(durationSeconds, 0) {
		durationSeconds = 0
	}
	return playbackTimeline{
		segmentDuration: segmentDuration,
		windowSegments:  windowSegments,
		durationSeconds: durationSeconds,
	}
}

func (t playbackTimeline) locate(seconds float64) playbackTarget {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		seconds = 0
	}
	if t.durationSeconds > 0 && seconds >= t.durationSeconds {
		seconds = math.Nextafter(t.durationSeconds, 0)
	}
	segment := int(math.Floor(seconds / t.segmentDuration.Seconds()))
	begin, end := t.windowForSegment(segment)
	return playbackTarget{
		sourceSeconds:       seconds,
		segment:             segment,
		windowBegin:         begin,
		windowEnd:           end,
		windowOriginSeconds: t.segmentStart(begin),
	}
}

func presentationOrigin(intervals []playbackInterval) (float64, bool) {
	origin := math.Inf(1)
	for _, interval := range intervals {
		if interval.durationSeconds > 0 && interval.startSeconds < origin {
			origin = interval.startSeconds
		}
	}
	return origin, !math.IsInf(origin, 1)
}

func (t playbackTimeline) segmentStart(index int) float64 {
	return float64(max(0, index)) * t.segmentDuration.Seconds()
}

func (t playbackTimeline) segmentEnd(index int) float64 {
	end := t.segmentStart(index + 1)
	if t.durationSeconds > 0 {
		end = min(end, t.durationSeconds)
	}
	return end
}

func (t playbackTimeline) containsSegment(index int) bool {
	if index < 0 {
		return false
	}
	total := t.segmentCount()
	return total == 0 || index < total
}

func (t playbackTimeline) windowForSegment(index int) (begin, end int) {
	index = max(0, index)
	begin = index / t.windowSegments * t.windowSegments
	end = begin + t.windowSegments
	if total := t.segmentCount(); total > 0 {
		end = min(end, total)
	}
	return begin, end
}

func (t playbackTimeline) segmentCount() int {
	if t.durationSeconds <= 0 {
		return 0
	}
	return int(math.Ceil(t.durationSeconds / t.segmentDuration.Seconds()))
}
