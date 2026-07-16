package ffmpeg

import (
	"cinemator/domain"
	"cinemator/infrastructure/cli"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	videoSegmentPrefix    = "chunk_"
	subtitleSegmentPrefix = "subs_"
	segmentNumberWidth    = 6
)

func UsesTextSubtitles(info domain.MediaInfo, selection StreamSelection) bool {
	index := selection.SubtitleTrackIndex
	return index >= 0 && index < len(info.Subtitles) && !isBitmapSubtitle(info.Subtitles[index].Codec)
}

func validateSelection(info domain.MediaInfo, selection StreamSelection) error {
	if selection.AudioTrackIndex < -1 {
		return fmt.Errorf("invalid audio track index: %d", selection.AudioTrackIndex)
	}
	if len(info.AudioTracks) > 0 && selection.AudioTrackIndex >= len(info.AudioTracks) {
		return fmt.Errorf("audio track index %d out of range (tracks: %d)", selection.AudioTrackIndex, len(info.AudioTracks))
	}
	if len(info.AudioTracks) == 0 && selection.AudioTrackIndex > 0 {
		return fmt.Errorf("audio track index %d out of range (no audio tracks)", selection.AudioTrackIndex)
	}
	if selection.SubtitleTrackIndex < -1 || selection.SubtitleTrackIndex >= len(info.Subtitles) {
		return fmt.Errorf("subtitle track index %d out of range (tracks: %d)", selection.SubtitleTrackIndex, len(info.Subtitles))
	}
	return nil
}

func PrepareOnDemandHLS(
	outDir, videoPlaylist, subtitlePlaylist, masterPlaylist string,
	info domain.MediaInfo,
	selection StreamSelection,
	segmentDuration time.Duration,
) error {
	if err := validateSelection(info, selection); err != nil {
		return err
	}
	seconds := segmentDuration.Seconds()
	if seconds <= 0 {
		return fmt.Errorf("segment duration must be positive")
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}

	video := ""
	if info.Duration > 0 {
		video = buildOnDemandMediaPlaylist(info.Duration, seconds, videoSegmentPrefix, ".ts")
	} else {
		video = buildProgressiveMediaPlaylist(seconds, videoSegmentPrefix, ".ts", 0, 0, false)
	}
	if err := writeFileAtomic(videoPlaylist, []byte(video), 0644); err != nil {
		return err
	}

	withSubtitles := UsesTextSubtitles(info, selection)
	if withSubtitles {
		subtitles := ""
		if info.Duration > 0 {
			subtitles = buildOnDemandMediaPlaylist(info.Duration, seconds, subtitleSegmentPrefix, ".vtt")
		} else {
			subtitles = buildProgressiveMediaPlaylist(seconds, subtitleSegmentPrefix, ".vtt", 0, 0, false)
		}
		if err := writeFileAtomic(subtitlePlaylist, []byte(subtitles), 0644); err != nil {
			return err
		}
	}

	language := ""
	if withSubtitles {
		language = info.Subtitles[selection.SubtitleTrackIndex].Language
	}
	master := buildMasterPlaylist(filepath.Base(videoPlaylist), filepath.Base(subtitlePlaylist), withSubtitles, language)
	return writeFileAtomic(masterPlaylist, []byte(master), 0644)
}

func UpdateProgressiveHLS(
	videoPlaylist, subtitlePlaylist string,
	withSubtitles bool,
	segmentDuration time.Duration,
	segmentCount int,
	lastDuration float64,
	ended bool,
) error {
	seconds := segmentDuration.Seconds()
	if seconds <= 0 || segmentCount < 0 {
		return fmt.Errorf("invalid progressive HLS state")
	}
	video := buildProgressiveMediaPlaylist(seconds, videoSegmentPrefix, ".ts", segmentCount, lastDuration, ended)
	if err := writeFileAtomic(videoPlaylist, []byte(video), 0644); err != nil {
		return err
	}
	if withSubtitles {
		subtitles := buildProgressiveMediaPlaylist(seconds, subtitleSegmentPrefix, ".vtt", segmentCount, lastDuration, ended)
		if err := writeFileAtomic(subtitlePlaylist, []byte(subtitles), 0644); err != nil {
			return err
		}
	}
	return nil
}

func UpdateOnDemandHLS(
	videoPlaylist, subtitlePlaylist string,
	withSubtitles bool,
	duration float64,
	segmentDuration time.Duration,
) error {
	seconds := segmentDuration.Seconds()
	if duration < 0 || seconds <= 0 {
		return fmt.Errorf("invalid on-demand HLS duration")
	}
	if err := writeFileAtomic(videoPlaylist, []byte(buildOnDemandMediaPlaylist(duration, seconds, videoSegmentPrefix, ".ts")), 0644); err != nil {
		return err
	}
	if withSubtitles {
		return writeFileAtomic(subtitlePlaylist, []byte(buildOnDemandMediaPlaylist(duration, seconds, subtitleSegmentPrefix, ".vtt")), 0644)
	}
	return nil
}

func buildOnDemandMediaPlaylist(duration, segmentDuration float64, prefix, suffix string) string {
	segments := int(math.Ceil(duration / segmentDuration))
	targetDuration := int(math.Ceil(segmentDuration))
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", targetDuration))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	for index := 0; index < segments; index++ {
		length := math.Min(segmentDuration, duration-float64(index)*segmentDuration)
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", length))
		b.WriteString(fmt.Sprintf("%s%0*d%s\n", prefix, segmentNumberWidth, index, suffix))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func buildProgressiveMediaPlaylist(segmentDuration float64, prefix, suffix string, segments int, lastDuration float64, ended bool) string {
	targetDuration := int(math.Ceil(segmentDuration))
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", targetDuration))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT\n")
	for index := 0; index < segments; index++ {
		length := segmentDuration
		if ended && index == segments-1 && lastDuration > 0 {
			length = lastDuration
		}
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", length))
		b.WriteString(fmt.Sprintf("%s%0*d%s\n", prefix, segmentNumberWidth, index, suffix))
	}
	if ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

type VideoWindowResult struct {
	Generated  int
	Durations  []float64
	ReachedEnd bool
}

func GenerateVideoWindow(
	ctx context.Context,
	inputURL, outDir string,
	info domain.MediaInfo,
	selection StreamSelection,
	firstSegment, segmentCount int,
	segmentDuration time.Duration,
	onPublished func(int),
) (VideoWindowResult, error) {
	if firstSegment < 0 || segmentCount <= 0 {
		return VideoWindowResult{}, fmt.Errorf("invalid HLS window")
	}
	start := time.Duration(firstSegment) * segmentDuration
	windowDuration := time.Duration(segmentCount) * segmentDuration
	if info.Duration > 0 {
		remaining := time.Duration(info.Duration*float64(time.Second)) - start
		if remaining < windowDuration {
			windowDuration = remaining
		}
	}
	if windowDuration <= 0 {
		return VideoWindowResult{}, fmt.Errorf("HLS window is outside media duration")
	}
	workDir, err := os.MkdirTemp(outDir, ".generating-")
	if err != nil {
		return VideoWindowResult{}, fmt.Errorf("create HLS generation directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	videoSelection := selection
	if UsesTextSubtitles(info, selection) {
		videoSelection.SubtitleTrackIndex = -1
	}
	args := []string{
		"-y",
		"-fflags", "+genpts",
		"-ss", formatDuration(start),
		"-i", inputURL,
		"-t", formatDuration(windowDuration),
	}
	args = append(args, buildStreamArgs(info, videoSelection)...)
	args = append(args, "-force_key_frames", "expr:gte(t,n_forced*"+formatSeconds(segmentDuration.Seconds())+")")
	args = append(args,
		"-output_ts_offset", formatDuration(start),
		"-f", "hls",
		"-hls_time", formatSeconds(segmentDuration.Seconds()),
		"-hls_list_size", "0",
		"-hls_flags", "split_by_time+temp_file",
		"-start_number", strconv.Itoa(firstSegment),
		"-hls_segment_filename", filepath.Join(workDir, videoSegmentPrefix+"%06d.ts"),
		"-muxdelay", "0",
		filepath.Join(workDir, "window.m3u8"),
	)
	publishDone := make(chan struct{})
	type publishOutcome struct {
		count int
		err   error
	}
	publishResult := make(chan publishOutcome, 1)
	go func() {
		outcome := publishOutcome{}
		defer func() {
			if recovered := recover(); recovered != nil {
				outcome.err = fmt.Errorf("publish HLS segment panic: %v", recovered)
			}
			publishResult <- outcome
		}()
		outcome.count, outcome.err = publishVideoSegments(ctx, workDir, outDir, firstSegment, firstSegment+segmentCount, publishDone, onPublished)
	}()
	_, runErr := cli.RunWithStdin(ctx, nil, "ffmpeg", args...)
	close(publishDone)
	published := <-publishResult
	if runErr != nil {
		return VideoWindowResult{}, runErr
	}
	if published.err != nil {
		return VideoWindowResult{}, published.err
	}
	durations, err := readGeneratedDurations(filepath.Join(workDir, "window.m3u8"), published.count)
	if err != nil && published.count > 0 {
		return VideoWindowResult{}, err
	}
	result := VideoWindowResult{
		Generated:  published.count,
		Durations:  durations,
		ReachedEnd: published.count < segmentCount,
	}
	if info.Duration <= 0 && len(durations) > 0 && durations[len(durations)-1] < segmentDuration.Seconds()-0.25 {
		result.ReachedEnd = true
	}
	return result, nil
}

func publishVideoSegments(ctx context.Context, workDir, outDir string, begin, end int, generationDone <-chan struct{}, onPublished func(int)) (int, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	next := begin
	publishAvailable := func(final bool) (bool, error) {
		for next < end {
			source := filepath.Join(workDir, videoSegmentPrefix+fmt.Sprintf("%06d.ts", next))
			if _, err := os.Stat(source); err != nil {
				if !os.IsNotExist(err) {
					return false, err
				}
				break
			}
			duration, found, err := generatedSegmentDuration(filepath.Join(workDir, "window.m3u8"), next-begin)
			if err != nil {
				return false, err
			}
			if !found || duration <= 0.05 {
				if final {
					_ = os.Remove(source)
					return true, nil
				}
				break
			}
			target := filepath.Join(outDir, videoSegmentPrefix+fmt.Sprintf("%06d.ts", next))
			if err := os.Rename(source, target); err != nil {
				return false, err
			}
			if onPublished != nil {
				onPublished(next)
			}
			next++
		}
		return false, nil
	}
	for {
		if _, err := publishAvailable(false); err != nil {
			return next - begin, err
		}
		if next == end {
			return next - begin, nil
		}
		select {
		case <-ctx.Done():
			return next - begin, ctx.Err()
		case <-generationDone:
			if _, err := publishAvailable(true); err != nil {
				return next - begin, err
			}
			return next - begin, nil
		case <-ticker.C:
		}
	}
}

func generatedSegmentDuration(path string, position int) (float64, bool, error) {
	if position < 0 {
		return 0, false, fmt.Errorf("invalid generated HLS position")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	current := 0
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
		duration, parseErr := strconv.ParseFloat(raw, 64)
		if parseErr != nil {
			return 0, false, fmt.Errorf("parse generated HLS duration %q: %w", raw, parseErr)
		}
		if current == position {
			return duration, true, nil
		}
		current++
	}
	return 0, false, nil
}

func readGeneratedDurations(path string, count int) ([]float64, error) {
	if count == 0 {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read generated HLS manifest: %w", err)
	}
	result := make([]float64, 0, count)
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
		duration, parseErr := strconv.ParseFloat(raw, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("parse generated HLS duration %q: %w", raw, parseErr)
		}
		result = append(result, duration)
		if len(result) == count {
			break
		}
	}
	if len(result) != count {
		return nil, fmt.Errorf("generated HLS manifest has %d durations for %d segments", len(result), count)
	}
	return result, nil
}

func GenerateSubtitleSegment(
	ctx context.Context,
	inputURL, outputPath string,
	subtitleTrack, segmentIndex int,
	segmentDuration time.Duration,
) error {
	if subtitleTrack < 0 || segmentIndex < 0 {
		return fmt.Errorf("invalid subtitle segment")
	}
	tmp := outputPath + ".tmp"
	_ = os.Remove(tmp)
	defer os.Remove(tmp)
	args := []string{
		"-y",
		"-fflags", "+genpts",
		"-ss", formatDuration(time.Duration(segmentIndex) * segmentDuration),
		"-i", inputURL,
		"-t", formatDuration(segmentDuration),
		"-map", fmt.Sprintf("0:s:%d", subtitleTrack),
		"-c:s", "webvtt",
		"-f", "webvtt",
		tmp,
	}
	if _, err := cli.RunWithStdin(ctx, nil, "ffmpeg", args...); err != nil {
		return err
	}
	if err := addWebVTTTimestampMap(tmp, time.Duration(segmentIndex)*segmentDuration); err != nil {
		return err
	}
	return os.Rename(tmp, outputPath)
}

func addWebVTTTimestampMap(path string, mediaOffset time.Duration) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lineEnd := strings.IndexByte(string(data), '\n')
	if lineEnd < 0 || strings.TrimSpace(string(data[:lineEnd])) != "WEBVTT" {
		return fmt.Errorf("generated subtitle segment has no WEBVTT header")
	}
	const ptsWrap = int64(1) << 33
	mpegTS := int64(math.Round(mediaOffset.Seconds()*90000)) % ptsWrap
	header := fmt.Sprintf("X-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:%d\n", mpegTS)
	mapped := make([]byte, 0, len(data)+len(header))
	mapped = append(mapped, data[:lineEnd+1]...)
	mapped = append(mapped, header...)
	mapped = append(mapped, data[lineEnd+1:]...)
	return os.WriteFile(path, mapped, 0644)
}

func formatDuration(value time.Duration) string {
	return formatSeconds(value.Seconds())
}

func formatSeconds(value float64) string {
	return strconv.FormatFloat(value, 'f', 3, 64)
}

func buildMasterPlaylist(videoList, subList string, withSubs bool, lang string) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	if withSubs {
		attributes := []string{
			"TYPE=SUBTITLES",
			"GROUP-ID=\"subs\"",
			"NAME=\"Subtitles\"",
			"DEFAULT=YES",
			"AUTOSELECT=YES",
			"FORCED=NO",
			fmt.Sprintf("URI=\"%s\"", subList),
		}
		if lang != "" {
			attributes = append(attributes, fmt.Sprintf("LANGUAGE=\"%s\"", lang))
		}
		b.WriteString("#EXT-X-MEDIA:" + strings.Join(attributes, ",") + "\n")
		b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=5500000,SUBTITLES=\"subs\"\n")
	} else {
		b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=5500000\n")
	}
	b.WriteString(videoList + "\n")
	return b.String()
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
