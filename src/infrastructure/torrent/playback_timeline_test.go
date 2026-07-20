package torrent

import (
	"math"
	"testing"
	"time"
)

func TestPlaybackTimelineLocatesCanonicalWindow(t *testing.T) {
	timeline := newPlaybackTimeline(2*time.Second, 15, 2*time.Hour.Seconds())
	target := timeline.locate(7199)

	if target.segment != 3599 || target.windowBegin != 3585 || target.windowEnd != 3600 || target.windowOriginSeconds != 7170 {
		t.Fatalf("locate(7199) = %+v", target)
	}
	if timeline.segmentCount() != 3600 {
		t.Fatalf("segmentCount() = %d, want 3600", timeline.segmentCount())
	}
}

func TestPlaybackTimelineUsesOpenEndedWindowForUnknownDuration(t *testing.T) {
	timeline := newPlaybackTimeline(2*time.Second, 15, 0)
	target := timeline.locate(31)

	if target.segment != 15 || target.windowBegin != 15 || target.windowEnd != 30 || target.windowOriginSeconds != 30 {
		t.Fatalf("locate(31) = %+v", target)
	}
	if timeline.segmentCount() != 0 {
		t.Fatalf("segmentCount() = %d, want unknown", timeline.segmentCount())
	}
}

func TestPlaybackTimelineClampsInvalidConfiguration(t *testing.T) {
	timeline := newPlaybackTimeline(0, 0, -1)
	target := timeline.locate(-5)

	if target.sourceSeconds != 0 || target.segment != 0 || target.windowBegin != 0 || target.windowEnd != 1 {
		t.Fatalf("locate(-5) = %+v", target)
	}
}

func TestPlaybackTimelineClampsKnownDurationToLastSegment(t *testing.T) {
	timeline := newPlaybackTimeline(2*time.Second, 15, 120)
	for _, seconds := range []float64{120, 121} {
		target := timeline.locate(seconds)
		if target.segment != 59 || target.windowBegin != 45 || target.windowEnd != 60 || !(target.sourceSeconds < 120) {
			t.Fatalf("locate(%v) = %+v", seconds, target)
		}
	}
}

func TestPlaybackTimelineSanitizesInvalidSourceTime(t *testing.T) {
	timeline := newPlaybackTimeline(2*time.Second, 15, 0)
	for _, seconds := range []float64{-1, math.NaN(), math.Inf(-1), math.Inf(1)} {
		target := timeline.locate(seconds)
		if target.sourceSeconds != 0 || target.segment != 0 || target.windowBegin != 0 {
			t.Fatalf("locate(%v) = %+v", seconds, target)
		}
	}
	if !timeline.containsSegment(10) || timeline.containsSegment(-1) {
		t.Fatal("unknown-duration segment bounds are incorrect")
	}
}

func TestPresentationOriginUsesFirstMaterializedInterval(t *testing.T) {
	origin, ok := presentationOrigin([]playbackInterval{
		{startSeconds: 63, durationSeconds: 2},
		{startSeconds: 31.5, durationSeconds: 2},
	})
	if !ok || origin != 31.5 {
		t.Fatalf("presentationOrigin() = %.3f, %t", origin, ok)
	}
}
