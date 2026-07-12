package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type subtitlePlaylistNormalizer struct {
	ctx               context.Context
	rawPlaylist       string
	outPlaylist       string
	videoList         string
	segmentDir        string
	subtitleCompleted <-chan struct{}
}

type playlistSegment struct {
	URI      string
	Duration float64
}

type subtitleCueBounds struct {
	firstStart float64
	lastEnd    float64
}

type normalizedSubtitleSegment struct {
	URI      string
	Start    float64
	End      float64
	Duration float64
}

var webVTTCueTimingRE = regexp.MustCompile(`^\s*((?:\d{2,}:)?\d{2}:\d{2}\.\d{3})\s+-->\s+((?:\d{2,}:)?\d{2}:\d{2}\.\d{3})`)

var ErrSubtitleTrackEmpty = errors.New("selected subtitle track has no cues")

const (
	subtitlePrerollFilename        = "subs_preroll.vtt"
	subtitlePrerollSegmentDuration = 4.0
)

func (n subtitlePlaylistNormalizer) run() error {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		done, err := n.refreshOnce()
		if err != nil {
			return err
		}
		if done {
			return nil
		}

		select {
		case <-n.ctx.Done():
			return n.ctx.Err()
		case <-ticker.C:
		}
	}
}

func (n subtitlePlaylistNormalizer) refreshOnce() (bool, error) {
	var rawSegments []playlistSegment
	rawEnded := false
	rawData, err := os.ReadFile(n.rawPlaylist)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if n.subtitleExtractionDone() {
				return false, ErrSubtitleTrackEmpty
			}
		} else {
			return false, err
		}
	} else {
		rawSegments, rawEnded, err = parseMediaPlaylist(rawData)
		if err != nil {
			return false, err
		}
	}

	videoDuration := 0.0
	videoEnded := false
	if videoData, err := os.ReadFile(n.videoList); err == nil {
		videoSegments, ended, err := parseMediaPlaylist(videoData)
		if err != nil {
			return false, err
		}
		videoDuration = playlistDuration(videoSegments)
		videoEnded = ended
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	playlist, hasSegments, complete, err := buildNormalizedSubtitlePlaylist(rawSegments, rawEnded, videoDuration, videoEnded, n.segmentDir)
	if err != nil {
		return false, err
	}
	if hasSegments {
		if err := writeFileAtomic(n.outPlaylist, []byte(playlist), 0644); err != nil {
			return false, err
		}
	}

	return rawEnded && videoEnded && complete, nil
}

func (n subtitlePlaylistNormalizer) subtitleExtractionDone() bool {
	if n.subtitleCompleted == nil {
		return false
	}
	select {
	case <-n.subtitleCompleted:
		return true
	default:
		return false
	}
}

func buildNormalizedSubtitlePlaylist(
	rawSegments []playlistSegment,
	rawEnded bool,
	videoDuration float64,
	videoEnded bool,
	segmentDir string,
) (string, bool, bool, error) {
	segments := make([]normalizedSubtitleSegment, 0, len(rawSegments))
	complete := true
	for _, raw := range rawSegments {
		bounds, ok, err := readSubtitleCueBounds(filepath.Join(segmentDir, raw.URI))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				complete = false
				break
			}
			return "", false, false, err
		}
		if !ok {
			continue
		}

		segments = append(segments, normalizedSubtitleSegment{
			URI:   raw.URI,
			Start: bounds.firstStart,
			End:   bounds.lastEnd,
		})
	}

	if len(segments) == 0 {
		if rawEnded && complete {
			return "", false, complete, ErrSubtitleTrackEmpty
		}
		if videoDuration > 0 {
			segments = appendSubtitlePreroll(segments, videoDuration)
			return renderSubtitlePlaylist(segments, false), true, complete, nil
		}
		return "", false, complete, nil
	}

	prerollDuration := math.Max(0, segments[0].Start)
	preroll := appendSubtitlePreroll(nil, prerollDuration)
	prerollCount := len(preroll)
	segments = append(preroll, segments...)

	for i := prerollCount; i < len(segments); i++ {
		nextStart := segments[i].End
		if i+1 < len(segments) {
			nextStart = segments[i+1].Start
		} else if rawEnded && videoEnded && videoDuration > segments[i].Start {
			nextStart = videoDuration
		}
		segments[i].Duration = math.Max(0.001, nextStart-segments[i].Start)
	}

	return renderSubtitlePlaylist(segments, rawEnded && videoEnded), true, complete, nil
}

func appendSubtitlePreroll(segments []normalizedSubtitleSegment, duration float64) []normalizedSubtitleSegment {
	if duration <= 0 {
		return segments
	}
	duration = math.Max(0.001, duration)
	start := 0.0
	for duration > subtitlePrerollSegmentDuration {
		if duration-subtitlePrerollSegmentDuration < 0.001 {
			duration = subtitlePrerollSegmentDuration
			break
		}
		segments = append(segments, normalizedSubtitleSegment{
			URI:      subtitlePrerollFilename,
			Start:    start,
			End:      start + subtitlePrerollSegmentDuration,
			Duration: subtitlePrerollSegmentDuration,
		})
		start += subtitlePrerollSegmentDuration
		duration -= subtitlePrerollSegmentDuration
	}
	return append(segments, normalizedSubtitleSegment{
		URI:      subtitlePrerollFilename,
		Start:    start,
		End:      start + duration,
		Duration: duration,
	})
}

func renderSubtitlePlaylist(segments []normalizedSubtitleSegment, ended bool) string {
	targetDuration := 1
	for _, seg := range segments {
		targetDuration = max(targetDuration, int(math.Ceil(seg.Duration)))
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", targetDuration))
	for _, seg := range segments {
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", seg.Duration))
		b.WriteString(seg.URI + "\n")
	}
	if ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

func parseMediaPlaylist(data []byte) ([]playlistSegment, bool, error) {
	var segments []playlistSegment
	var pendingDuration *float64
	ended := false

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if line == "#EXT-X-ENDLIST" {
			ended = true
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			duration, err := parseEXTINFDuration(line)
			if err != nil {
				return nil, false, err
			}
			pendingDuration = &duration
			continue
		}
		if pendingDuration == nil || strings.HasPrefix(line, "#") {
			continue
		}
		segments = append(segments, playlistSegment{
			URI:      line,
			Duration: *pendingDuration,
		})
		pendingDuration = nil
	}

	return segments, ended, nil
}

func parseEXTINFDuration(line string) (float64, error) {
	raw := strings.TrimPrefix(line, "#EXTINF:")
	if comma := strings.IndexByte(raw, ','); comma >= 0 {
		raw = raw[:comma]
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("parse EXTINF duration %q: %w", line, err)
	}
	return duration, nil
}

func playlistDuration(segments []playlistSegment) float64 {
	total := 0.0
	for _, seg := range segments {
		total += seg.Duration
	}
	return total
}

func readSubtitleCueBounds(path string) (subtitleCueBounds, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return subtitleCueBounds{}, false, err
	}

	var bounds subtitleCueBounds
	found := false
	for _, raw := range strings.Split(string(data), "\n") {
		match := webVTTCueTimingRE.FindStringSubmatch(raw)
		if match == nil {
			continue
		}
		start, err := parseWebVTTTime(match[1])
		if err != nil {
			return subtitleCueBounds{}, false, err
		}
		end, err := parseWebVTTTime(match[2])
		if err != nil {
			return subtitleCueBounds{}, false, err
		}
		if !found {
			bounds.firstStart = start
			found = true
		}
		bounds.lastEnd = end
	}

	return bounds, found, nil
}

func parseWebVTTTime(raw string) (float64, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, fmt.Errorf("bad WebVTT timestamp %q", raw)
	}

	seconds, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("bad WebVTT timestamp %q: %w", raw, err)
	}
	minutes, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		return 0, fmt.Errorf("bad WebVTT timestamp %q: %w", raw, err)
	}

	hours := 0
	if len(parts) == 3 {
		hours, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, fmt.Errorf("bad WebVTT timestamp %q: %w", raw, err)
		}
	}

	return float64(hours*3600+minutes*60) + seconds, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
