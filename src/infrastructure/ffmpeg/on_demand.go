package ffmpeg

import (
	"cinemator/domain"
	"cinemator/infrastructure/cli"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	videoSegmentPrefix     = "chunk_"
	directSegmentPrefix    = "direct_"
	seekSegmentPrefix      = "seek_"
	subtitleSegmentPrefix  = "subs_"
	segmentNumberWidth     = 6
	maxDirectPreroll       = 30 * time.Second
	generationPollInterval = 100 * time.Millisecond
)

var ErrRemuxNeedsTranscode = errors.New("direct HLS window needs transcoding")

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
	windowSegments int,
	assetVersion string,
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
	if CanRemuxHLS(info, selection) {
		video = buildSparseMediaPlaylist(info.Duration, seconds, windowSegments, assetVersion, nil)
	} else if info.Duration > 0 {
		video = buildOnDemandMediaPlaylist(info.Duration, seconds, windowSegments, videoSegmentPrefix, ".ts", assetVersion)
	} else {
		video = buildProgressiveMediaPlaylist(seconds, windowSegments, videoSegmentPrefix, ".ts", assetVersion, 0, 0, false)
	}
	if err := writeFileAtomic(videoPlaylist, []byte(video), 0644); err != nil {
		return err
	}

	withSubtitles := UsesTextSubtitles(info, selection)
	if withSubtitles {
		subtitles := ""
		if info.Duration > 0 {
			subtitles = buildOnDemandMediaPlaylist(info.Duration, seconds, windowSegments, subtitleSegmentPrefix, ".vtt", assetVersion)
		} else {
			subtitles = buildProgressiveMediaPlaylist(seconds, windowSegments, subtitleSegmentPrefix, ".vtt", assetVersion, 0, 0, false)
		}
		if err := writeFileAtomic(subtitlePlaylist, []byte(subtitles), 0644); err != nil {
			return err
		}
	}

	language := ""
	if withSubtitles {
		language = info.Subtitles[selection.SubtitleTrackIndex].Language
	}
	master := buildMasterPlaylist(filepath.Base(videoPlaylist), filepath.Base(subtitlePlaylist), withSubtitles, language, assetVersion)
	return writeFileAtomic(masterPlaylist, []byte(master), 0644)
}

func UpdateProgressiveHLS(
	videoPlaylist, subtitlePlaylist string,
	withSubtitles bool,
	segmentDuration time.Duration,
	windowSegments int,
	assetVersion string,
	segmentCount int,
	lastDuration float64,
	ended bool,
) error {
	seconds := segmentDuration.Seconds()
	if seconds <= 0 || segmentCount < 0 {
		return fmt.Errorf("invalid progressive HLS state")
	}
	video := buildProgressiveMediaPlaylist(seconds, windowSegments, videoSegmentPrefix, ".ts", assetVersion, segmentCount, lastDuration, ended)
	if err := writeFileAtomic(videoPlaylist, []byte(video), 0644); err != nil {
		return err
	}
	if withSubtitles {
		subtitles := buildProgressiveMediaPlaylist(seconds, windowSegments, subtitleSegmentPrefix, ".vtt", assetVersion, segmentCount, lastDuration, ended)
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
	windowSegments int,
	assetVersion string,
) error {
	seconds := segmentDuration.Seconds()
	if duration < 0 || seconds <= 0 {
		return fmt.Errorf("invalid on-demand HLS duration")
	}
	if err := writeFileAtomic(videoPlaylist, []byte(buildOnDemandMediaPlaylist(duration, seconds, windowSegments, videoSegmentPrefix, ".ts", assetVersion)), 0644); err != nil {
		return err
	}
	if withSubtitles {
		return writeFileAtomic(subtitlePlaylist, []byte(buildOnDemandMediaPlaylist(duration, seconds, windowSegments, subtitleSegmentPrefix, ".vtt", assetVersion)), 0644)
	}
	return nil
}

func buildOnDemandMediaPlaylist(duration, segmentDuration float64, windowSegments int, prefix, suffix, assetVersion string) string {
	segments := int(math.Ceil(duration / segmentDuration))
	targetDuration := int(math.Ceil(segmentDuration))
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", targetDuration))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	for index := 0; index < segments; index++ {
		writeWindowDiscontinuity(&b, index, windowSegments)
		length := math.Min(segmentDuration, duration-float64(index)*segmentDuration)
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", length))
		b.WriteString(versionedAsset(fmt.Sprintf("%s%0*d%s", prefix, segmentNumberWidth, index, suffix), assetVersion) + "\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func buildProgressiveMediaPlaylist(segmentDuration float64, windowSegments int, prefix, suffix, assetVersion string, segments int, lastDuration float64, ended bool) string {
	targetDuration := int(math.Ceil(segmentDuration))
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", targetDuration))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT\n")
	for index := 0; index < segments; index++ {
		writeWindowDiscontinuity(&b, index, windowSegments)
		length := segmentDuration
		if ended && index == segments-1 && lastDuration > 0 {
			length = lastDuration
		}
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", length))
		b.WriteString(versionedAsset(fmt.Sprintf("%s%0*d%s", prefix, segmentNumberWidth, index, suffix), assetVersion) + "\n")
	}
	if ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

func writeWindowDiscontinuity(b *strings.Builder, index, windowSegments int) {
	if index > 0 && windowSegments > 0 && index%windowSegments == 0 {
		b.WriteString("#EXT-X-DISCONTINUITY\n")
	}
}

func versionedAsset(name, version string) string {
	if version == "" {
		return name
	}
	return name + "?v=" + version
}

type VideoWindowResult struct {
	Generated  int
	Durations  []float64
	ReachedEnd bool
}

type HLSFragment struct {
	Start    float64
	Duration float64
	Name     string
}

type DirectWindowResult struct {
	Fragments []HLSFragment
}

// DirectWindowGenerationDuration is the maximum input span a direct window
// reads while looking for its closing keyframe.
func DirectWindowGenerationDuration(segmentCount int, segmentDuration time.Duration) time.Duration {
	if segmentCount <= 0 || segmentDuration <= 0 {
		return 0
	}
	return time.Duration(segmentCount)*segmentDuration + maxDirectPreroll
}

func UpdateDirectHLS(videoPlaylist string, duration float64, segmentDuration time.Duration, windowSegments int, assetVersion string, fragments []HLSFragment) error {
	if duration <= 0 || segmentDuration <= 0 || windowSegments <= 0 {
		return fmt.Errorf("invalid direct HLS timeline")
	}
	playlist := buildSparseMediaPlaylist(duration, segmentDuration.Seconds(), windowSegments, assetVersion, fragments)
	return writeFileAtomic(videoPlaylist, []byte(playlist), 0644)
}

func buildSparseMediaPlaylist(duration, segmentDuration float64, windowSegments int, assetVersion string, fragments []HLSFragment) string {
	windowSegments = max(1, windowSegments)
	triggerDuration := segmentDuration * float64(windowSegments)
	timelineEnd := duration
	prepared := append([]HLSFragment(nil), fragments...)
	sort.Slice(prepared, func(i, j int) bool {
		if math.Abs(prepared[i].Start-prepared[j].Start) > 0.001 {
			return prepared[i].Start < prepared[j].Start
		}
		return prepared[i].Duration > prepared[j].Duration
	})
	for _, fragment := range prepared {
		timelineEnd = max(timelineEnd, fragment.Start+fragment.Duration)
	}

	entries := make([]HLSFragment, 0, len(prepared)+int(math.Ceil(timelineEnd/triggerDuration)))
	appendGap := func(begin, end float64) {
		for begin < end-0.001 {
			segment := int(math.Floor((begin + 0.001) / segmentDuration))
			index := segment / windowSegments * windowSegments
			next := math.Min(end, float64(index+windowSegments)*segmentDuration)
			if next <= begin+0.001 {
				next = math.Min(end, begin+triggerDuration)
			}
			entries = append(entries, HLSFragment{
				Start:    begin,
				Duration: next - begin,
				Name:     fmt.Sprintf("%s%0*d.ts", seekSegmentPrefix, segmentNumberWidth, index),
			})
			begin = next
		}
	}

	cursor := 0.0
	for _, fragment := range prepared {
		if gap := fragment.Start - cursor; gap > 0.001 && gap <= 0.25 {
			fragment.Start = cursor
			fragment.Duration += gap
		}
		end := fragment.Start + fragment.Duration
		if fragment.Duration <= 0.001 || fragment.Name == "" || end <= cursor+0.001 {
			continue
		}
		// HLS fragments cannot be cropped. Overlapping windows emit the same
		// source GOPs, so keeping the first complete copy preserves continuity.
		if fragment.Start < cursor-0.001 {
			continue
		}
		if fragment.Start > cursor+0.001 {
			appendGap(cursor, fragment.Start)
		}
		entries = append(entries, fragment)
		cursor = end
	}
	if cursor < timelineEnd-0.001 {
		appendGap(cursor, timelineEnd)
	}

	targetDuration := int(math.Ceil(triggerDuration))
	for _, entry := range entries {
		targetDuration = max(targetDuration, int(math.Ceil(entry.Duration)))
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", targetDuration))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	for _, entry := range entries {
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", entry.Duration))
		b.WriteString(versionedAsset(entry.Name, assetVersion) + "\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func GenerateDirectWindow(
	ctx context.Context,
	inputURL, outDir string,
	info domain.MediaInfo,
	selection StreamSelection,
	firstSegment, segmentCount int,
	segmentDuration time.Duration,
) (DirectWindowResult, error) {
	if !CanRemuxHLS(info, selection) {
		return DirectWindowResult{}, ErrRemuxNeedsTranscode
	}
	if firstSegment < 0 || segmentCount <= 0 || segmentDuration <= 0 {
		return DirectWindowResult{}, fmt.Errorf("invalid direct HLS window")
	}
	start := time.Duration(firstSegment) * segmentDuration
	wantedEnd := start + time.Duration(segmentCount)*segmentDuration
	if remaining := time.Duration(info.Duration*float64(time.Second)) - start; remaining <= 0 {
		return DirectWindowResult{}, fmt.Errorf("HLS window is outside media duration")
	} else if wantedEnd > start+remaining {
		wantedEnd = start + remaining
	}
	readDuration := DirectWindowGenerationDuration(segmentCount, segmentDuration)
	if remaining := time.Duration(info.Duration*float64(time.Second)) - start; readDuration > remaining {
		readDuration = remaining
	}

	workDir, err := os.MkdirTemp(outDir, ".remuxing-")
	if err != nil {
		return DirectWindowResult{}, fmt.Errorf("create direct HLS generation directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-y",
		"-fflags", "+genpts",
		"-ss", formatDuration(start),
		"-i", inputURL,
		"-t", formatDuration(readDuration),
	}
	args = append(args, buildRemuxStreamArgs(info, selection)...)
	args = append(args,
		"-sn", "-dn",
		"-output_ts_offset", formatDuration(start),
		"-f", "hls",
		"-hls_time", formatDuration(segmentDuration),
		"-hls_list_size", "0",
		"-hls_flags", "temp_file",
		"-start_number", "0",
		"-hls_segment_filename", filepath.Join(workDir, "part_%06d.ts"),
		"-muxdelay", "0",
		filepath.Join(workDir, "window.m3u8"),
	)
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	stdin, stopFFmpeg := io.Pipe()
	defer stdin.Close()
	defer stopFFmpeg.Close()
	covered := make(chan struct{}, 1)
	go func() {
		if waitForDirectCoverage(monitorCtx, workDir, wantedEnd.Seconds()) {
			covered <- struct{}{}
			_, _ = io.WriteString(stopFFmpeg, "q\n")
			_ = stopFFmpeg.Close()
		}
	}()
	_, runErr := cli.RunWithStdin(ctx, stdin, "ffmpeg", args...)
	stopMonitor()
	wasCovered := false
	select {
	case <-covered:
		wasCovered = true
	default:
	}
	if runErr != nil && !wasCovered {
		if ctx.Err() != nil {
			return DirectWindowResult{}, runErr
		}
		return DirectWindowResult{}, fmt.Errorf("%w: %v", ErrRemuxNeedsTranscode, runErr)
	}
	durations, err := readAllGeneratedDurations(filepath.Join(workDir, "window.m3u8"))
	if err != nil {
		return DirectWindowResult{}, err
	}
	if len(durations) == 0 {
		return DirectWindowResult{}, fmt.Errorf("%w: FFmpeg produced no GOPs", ErrRemuxNeedsTranscode)
	}
	firstPTS, err := probeFirstVideoPTS(ctx, filepath.Join(workDir, "part_000000.ts"))
	if err != nil {
		return DirectWindowResult{}, fmt.Errorf("%w: %v", ErrRemuxNeedsTranscode, err)
	}
	startSeconds := start.Seconds()
	if firstPTS > startSeconds+0.25 || startSeconds-firstPTS > maxDirectPreroll.Seconds()+0.25 {
		return DirectWindowResult{}, fmt.Errorf("%w: nearest keyframe is %.3fs from target", ErrRemuxNeedsTranscode, startSeconds-firstPTS)
	}

	wantedEndSeconds := wantedEnd.Seconds()
	cursor := firstPTS
	last := -1
	for index, duration := range durations {
		cursor += duration
		complete := index+1 < len(durations) || wasCovered || cursor >= info.Duration-0.25
		if complete && cursor >= wantedEndSeconds-0.05 {
			last = index
			break
		}
	}
	if last < 0 {
		return DirectWindowResult{}, fmt.Errorf("%w: no closing keyframe within %s", ErrRemuxNeedsTranscode, maxDirectPreroll)
	}

	result := DirectWindowResult{Fragments: make([]HLSFragment, 0, last+1)}
	cursor = firstPTS
	for index := 0; index <= last; index++ {
		name := fmt.Sprintf("%s%0*d_%04d.ts", directSegmentPrefix, segmentNumberWidth, firstSegment, index)
		if err := os.Rename(filepath.Join(workDir, fmt.Sprintf("part_%06d.ts", index)), filepath.Join(outDir, name)); err != nil {
			return DirectWindowResult{}, fmt.Errorf("publish direct HLS segment: %w", err)
		}
		result.Fragments = append(result.Fragments, HLSFragment{
			Start:    cursor,
			Duration: durations[index],
			Name:     name,
		})
		cursor += durations[index]
	}
	return result, nil
}

func waitForDirectCoverage(ctx context.Context, workDir string, wantedEnd float64) bool {
	ticker := time.NewTicker(generationPollInterval)
	defer ticker.Stop()
	firstPTS := math.NaN()
	for {
		durations, err := readAllGeneratedDurations(filepath.Join(workDir, "window.m3u8"))
		if err == nil && len(durations) > 0 {
			if math.IsNaN(firstPTS) {
				firstPTS, err = probeFirstVideoPTS(ctx, filepath.Join(workDir, "part_000000.ts"))
			}
			if err == nil {
				end := firstPTS
				for _, duration := range durations {
					end += duration
				}
				if end >= wantedEnd-0.05 {
					return true
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
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
		"-hide_banner", "-loglevel", "error", "-nostats",
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
	ticker := time.NewTicker(generationPollInterval)
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

func readAllGeneratedDurations(path string) ([]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read generated HLS manifest: %w", err)
	}
	var result []float64
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
		duration, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("parse generated HLS duration %q: %w", raw, err)
		}
		if duration > 0.001 {
			result = append(result, duration)
		}
	}
	return result, nil
}

func probeFirstVideoPTS(ctx context.Context, path string) (float64, error) {
	out, err := cli.RunWithStdin(ctx, nil,
		"ffprobe", "-v", "error",
		"-select_streams", "v:0",
		"-read_intervals", "%+0.1",
		"-show_entries", "packet=pts_time",
		"-of", "default=nw=1:nk=1",
		path,
	)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		value := strings.TrimSpace(line)
		if value == "" {
			continue
		}
		pts, err := strconv.ParseFloat(value, 64)
		if err == nil {
			return pts, nil
		}
	}
	return 0, fmt.Errorf("FFprobe returned no video timestamp")
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
		"-hide_banner", "-loglevel", "error", "-nostats",
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

func buildMasterPlaylist(videoList, subList string, withSubs bool, lang, assetVersion string) string {
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
			fmt.Sprintf("URI=\"%s\"", versionedAsset(subList, assetVersion)),
		}
		if lang != "" {
			attributes = append(attributes, fmt.Sprintf("LANGUAGE=\"%s\"", lang))
		}
		b.WriteString("#EXT-X-MEDIA:" + strings.Join(attributes, ",") + "\n")
		b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=5500000,SUBTITLES=\"subs\"\n")
	} else {
		b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=5500000\n")
	}
	b.WriteString(versionedAsset(videoList, assetVersion) + "\n")
	return b.String()
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
