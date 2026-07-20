package ffmpeg

import (
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

	"cinemator/domain"
	"cinemator/infrastructure/cli"
)

const (
	videoSegmentPrefix     = "chunk_"
	directSegmentPrefix    = "direct_"
	subtitleSegmentPrefix  = "subs_"
	segmentNumberWidth     = 6
	generationPollInterval = 100 * time.Millisecond
)

var ErrRemuxNeedsTranscode = errors.New("direct HLS window needs transcoding")

func UsesTextSubtitles(info domain.MediaInfo, selection StreamSelection) bool {
	index := selection.SubtitleTrackIndex
	return index >= 0 && index < len(info.Subtitles) && !isBitmapSubtitle(info.Subtitles[index].Codec)
}

func validateSelection(info domain.MediaInfo, selection StreamSelection) error {
	if selection.AudioTrackIndex < -1 {
		return fmt.Errorf("%w: invalid audio track index: %d", domain.ErrBadHlsRequest, selection.AudioTrackIndex)
	}
	if len(info.AudioTracks) > 0 && selection.AudioTrackIndex >= len(info.AudioTracks) {
		return fmt.Errorf("%w: audio track index %d out of range (tracks: %d)", domain.ErrBadHlsRequest, selection.AudioTrackIndex, len(info.AudioTracks))
	}
	if len(info.AudioTracks) == 0 && selection.AudioTrackIndex > 0 {
		return fmt.Errorf("%w: audio track index %d out of range (no audio tracks)", domain.ErrBadHlsRequest, selection.AudioTrackIndex)
	}
	if selection.SubtitleTrackIndex < -1 || selection.SubtitleTrackIndex >= len(info.Subtitles) {
		return fmt.Errorf("%w: subtitle track index %d out of range (tracks: %d)", domain.ErrBadHlsRequest, selection.SubtitleTrackIndex, len(info.Subtitles))
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

	videoTarget := 2 * segmentDuration
	fmp4 := false
	if CanRemuxHLS(info, selection) {
		fmp4 = UsesFMP4(info, selection)
	}
	video := buildMaterializedPlaylist(videoTarget, assetVersion, fmp4, 0, 0, 0, false, nil)
	if err := writeFileAtomic(videoPlaylist, []byte(video), 0644); err != nil {
		return err
	}

	withSubtitles := UsesTextSubtitles(info, selection)
	if withSubtitles {
		segmentCount := 0
		lastDuration := 0.0
		ended := false
		if info.Duration > 0 {
			segmentCount = int(math.Ceil(info.Duration / seconds))
			lastDuration = info.Duration - float64(segmentCount-1)*seconds
			ended = true
			windowSegments = max(windowSegments, segmentCount)
		}
		subtitles := buildProgressiveMediaPlaylist(seconds, windowSegments, subtitleSegmentPrefix, ".vtt", assetVersion, segmentCount, lastDuration, ended)
		if err := writeFileAtomic(subtitlePlaylist, []byte(subtitles), 0644); err != nil {
			return err
		}
	}

	language := ""
	if withSubtitles {
		language = info.Subtitles[selection.SubtitleTrackIndex].Language
	}
	master := buildMasterPlaylist(filepath.Base(videoPlaylist), filepath.Base(subtitlePlaylist), withSubtitles, language, assetVersion, info, selection, segmentDuration)
	return writeFileAtomic(masterPlaylist, []byte(master), 0644)
}

func UpdateProgressiveSubtitleHLS(
	subtitlePlaylist string,
	segmentDuration time.Duration,
	windowSegments int,
	assetVersion string,
	segmentCount int,
	lastDuration float64,
	ended bool,
) error {
	seconds := segmentDuration.Seconds()
	if seconds <= 0 || segmentCount < 0 {
		return fmt.Errorf("invalid progressive subtitle HLS state")
	}
	subtitles := buildProgressiveMediaPlaylist(seconds, windowSegments, subtitleSegmentPrefix, ".vtt", assetVersion, segmentCount, lastDuration, ended)
	return writeFileAtomic(subtitlePlaylist, []byte(subtitles), 0644)
}

func buildProgressiveMediaPlaylist(segmentDuration float64, windowSegments int, prefix, suffix, assetVersion string, segments int, lastDuration float64, ended bool) string {
	discontinuityWindow := max(1, windowSegments)
	first := max(0, segments-max(3, windowSegments))
	targetDuration := int(math.Ceil(segmentDuration))
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", targetDuration))
	b.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", first))
	b.WriteString(fmt.Sprintf("#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", first/discontinuityWindow))
	for index := first; index < segments; index++ {
		if index > first {
			writeWindowDiscontinuity(&b, index, discontinuityWindow)
		}
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
	Start         float64
	Duration      float64
	Name          string
	Init          string
	Discontinuity bool
}

type DirectWindowResult struct {
	Fragments  []HLSFragment
	ReachedEnd bool
}

type directPublishOutcome struct {
	fragments []HLSFragment
	firstPTS  float64
	covered   bool
	err       error
}

// DirectWindowGenerationDuration is the maximum input span a direct window
// reads while looking for its closing keyframe.
func DirectWindowGenerationDuration(segmentCount int, segmentDuration, prerollBudget time.Duration) time.Duration {
	if segmentCount <= 0 || segmentDuration <= 0 || prerollBudget < 0 {
		return 0
	}
	return time.Duration(segmentCount)*segmentDuration + prerollBudget
}

// UpdateMaterializedHLS publishes only complete media fragments. The caller
// owns the bounded tail and its media-sequence value.
func UpdateMaterializedHLS(
	videoPlaylist string,
	targetDuration time.Duration,
	assetVersion string,
	fmp4 bool,
	mediaSequence int,
	discontinuitySequence int,
	sourceTarget float64,
	ended bool,
	fragments []HLSFragment,
) error {
	if targetDuration <= 0 || mediaSequence < 0 || discontinuitySequence < 0 || math.IsNaN(sourceTarget) || math.IsInf(sourceTarget, 0) {
		return fmt.Errorf("invalid materialized HLS state")
	}
	playlist := buildMaterializedPlaylist(
		targetDuration,
		assetVersion,
		fmp4,
		mediaSequence,
		discontinuitySequence,
		sourceTarget,
		ended,
		fragments,
	)
	return writeFileAtomic(videoPlaylist, []byte(playlist), 0644)
}

func buildMaterializedPlaylist(
	targetDuration time.Duration,
	assetVersion string,
	fmp4 bool,
	mediaSequence int,
	discontinuitySequence int,
	sourceTarget float64,
	ended bool,
	fragments []HLSFragment,
) string {
	fragments = append([]HLSFragment(nil), fragments...)
	sort.Slice(fragments, func(i, j int) bool {
		if math.Abs(fragments[i].Start-fragments[j].Start) > 0.001 {
			return fragments[i].Start < fragments[j].Start
		}
		return fragments[i].Duration > fragments[j].Duration
	})
	prepared := fragments[:0]
	cursor := math.Inf(-1)
	for _, fragment := range fragments {
		if fragment.Duration <= 0.001 || fragment.Name == "" {
			continue
		}
		if !math.IsInf(cursor, -1) {
			if overlap := cursor - fragment.Start; overlap > 0.001 && overlap <= 0.25 {
				fragment.Start = cursor
			}
			if fragment.Start < cursor-0.001 {
				continue
			}
		}
		prepared = append(prepared, fragment)
		cursor = fragment.Start + fragment.Duration
	}

	targetSeconds := int(math.Ceil(targetDuration.Seconds()))
	for _, fragment := range prepared {
		targetSeconds = max(targetSeconds, int(math.Ceil(fragment.Duration)))
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	if fmp4 {
		b.WriteString("#EXT-X-VERSION:7\n")
	} else {
		b.WriteString("#EXT-X-VERSION:3\n")
	}
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", targetSeconds))
	b.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", mediaSequence))
	b.WriteString(fmt.Sprintf("#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", discontinuitySequence))
	if offset, ok := playlistStartOffset(prepared, sourceTarget); ok {
		b.WriteString(fmt.Sprintf("#EXT-X-START:TIME-OFFSET=%.3f,PRECISE=YES\n", offset))
	}
	currentInit := ""
	previousEnd := math.Inf(-1)
	for _, fragment := range prepared {
		if !math.IsInf(previousEnd, -1) && (fragment.Discontinuity || fragment.Start > previousEnd+0.25 || fragment.Init != "" && currentInit != "" && fragment.Init != currentInit) {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if fragment.Init != "" && fragment.Init != currentInit {
			b.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"%s\"\n", versionedAsset(fragment.Init, assetVersion)))
			currentInit = fragment.Init
		}
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", fragment.Duration))
		b.WriteString(versionedAsset(fragment.Name, assetVersion) + "\n")
		previousEnd = fragment.Start + fragment.Duration
	}
	if ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

func playlistStartOffset(fragments []HLSFragment, sourceTarget float64) (float64, bool) {
	offset := 0.0
	for _, fragment := range fragments {
		end := fragment.Start + fragment.Duration
		if sourceTarget >= fragment.Start-0.001 && sourceTarget < end-0.001 {
			return offset + max(0, sourceTarget-fragment.Start), true
		}
		offset += fragment.Duration
	}
	return 0, false
}

func GenerateDirectWindow(
	ctx context.Context,
	inputURL, outDir string,
	info domain.MediaInfo,
	selection StreamSelection,
	firstSegment, segmentCount int,
	segmentDuration, prerollBudget time.Duration,
	onPublished func(HLSFragment) error,
) (DirectWindowResult, error) {
	if !CanRemuxHLS(info, selection) {
		return DirectWindowResult{}, ErrRemuxNeedsTranscode
	}
	if firstSegment < 0 || segmentCount <= 0 || segmentDuration <= 0 || prerollBudget < 0 {
		return DirectWindowResult{}, fmt.Errorf("invalid direct HLS window")
	}
	start := time.Duration(firstSegment) * segmentDuration
	wantedEnd := start + time.Duration(segmentCount)*segmentDuration
	if info.Duration > 0 {
		if remaining := time.Duration(info.Duration*float64(time.Second)) - start; remaining <= 0 {
			return DirectWindowResult{}, fmt.Errorf("HLS window is outside media duration")
		} else if wantedEnd > start+remaining {
			wantedEnd = start + remaining
		}
	}
	readDuration := DirectWindowGenerationDuration(segmentCount, segmentDuration, prerollBudget)

	workDir, err := os.MkdirTemp(outDir, ".remuxing-")
	if err != nil {
		return DirectWindowResult{}, fmt.Errorf("create direct HLS generation directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	fmp4 := UsesFMP4(info, selection)
	segmentExtension := ".ts"
	if fmp4 {
		segmentExtension = ".m4s"
	}

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
		"-start_number", "0",
	)
	if fmp4 {
		args = append(args,
			"-hls_flags", "temp_file+split_by_time",
			"-hls_segment_type", "fmp4",
			"-hls_fmp4_init_filename", "init.mp4",
		)
	} else {
		args = append(args, "-hls_flags", "temp_file+split_by_time")
	}
	args = append(args,
		"-hls_segment_filename", filepath.Join(workDir, "part_%06d"+segmentExtension),
		"-muxdelay", "0",
		filepath.Join(workDir, "window.m3u8"),
	)
	wasCovered := false
	published := directPublishOutcome{firstPTS: math.NaN()}
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	stdin, stopFFmpeg, pipeErr := os.Pipe()
	if pipeErr != nil {
		stopMonitor()
		return DirectWindowResult{}, fmt.Errorf("create FFmpeg control pipe: %w", pipeErr)
	}
	publishResult := make(chan directPublishOutcome, 1)
	go func() {
		outcome := publishDirectCoverage(
			monitorCtx,
			workDir,
			outDir,
			firstSegment,
			start.Seconds(),
			wantedEnd.Seconds(),
			prerollBudget,
			fmp4,
			onPublished,
		)
		publishResult <- outcome
		if outcome.covered || outcome.err != nil {
			_, _ = io.WriteString(stopFFmpeg, "q\n")
			_ = stopFFmpeg.Close()
		}
	}()
	_, runErr := cli.RunWithStdin(ctx, stdin, "ffmpeg", args...)
	_ = stdin.Close()
	_ = stopFFmpeg.Close()
	stopMonitor()
	published = <-publishResult
	wasCovered = published.covered
	if published.err != nil {
		return DirectWindowResult{}, published.err
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
	firstPTS := published.firstPTS
	if math.IsNaN(firstPTS) {
		firstPTS, err = probeFirstDirectVideoPTS(ctx, workDir, fmp4)
		if err != nil {
			return DirectWindowResult{}, fmt.Errorf("%w: %v", ErrRemuxNeedsTranscode, err)
		}
	}
	startSeconds := start.Seconds()
	if firstPTS > startSeconds+0.25 || startSeconds-firstPTS > prerollBudget.Seconds()+0.25 {
		return DirectWindowResult{}, fmt.Errorf("%w: nearest keyframe is %.3fs from target", ErrRemuxNeedsTranscode, startSeconds-firstPTS)
	}

	wantedEndSeconds := wantedEnd.Seconds()
	totalEnd := firstPTS
	for _, duration := range durations {
		totalEnd += duration
	}
	reachedFinalWindowEnd := totalEnd >= info.Duration-0.25 &&
		info.Duration-wantedEndSeconds <= segmentDuration.Seconds()+0.25
	reachedKnownEnd := info.Duration > 0 && (wantedEndSeconds >= info.Duration-0.25 ||
		reachedFinalWindowEnd || totalEnd < wantedEndSeconds-0.25)
	reachedUnknownEnd := info.Duration <= 0 && runErr == nil && !wasCovered &&
		totalEnd < (start+readDuration).Seconds()-0.25
	reachedEnd := reachedKnownEnd || reachedUnknownEnd
	cursor := firstPTS
	last := -1
	if reachedEnd {
		last = len(durations) - 1
	} else {
		for index, duration := range durations {
			cursor += duration
			complete := index+1 < len(durations) || wasCovered || info.Duration > 0 && cursor >= info.Duration-0.25
			if complete && cursor >= wantedEndSeconds-0.05 {
				last = index
				break
			}
		}
	}
	if last < 0 {
		return DirectWindowResult{}, fmt.Errorf("%w: no closing keyframe within admitted %s preroll", ErrRemuxNeedsTranscode, prerollBudget)
	}

	result := DirectWindowResult{
		Fragments:  append(make([]HLSFragment, 0, last+1), published.fragments...),
		ReachedEnd: reachedEnd,
	}
	initName := ""
	if fmp4 {
		initName = fmt.Sprintf("init_%0*d.mp4", segmentNumberWidth, firstSegment)
		if len(result.Fragments) == 0 {
			if err := publishFileWithoutReplacement(filepath.Join(workDir, "init.mp4"), filepath.Join(outDir, initName)); err != nil {
				return DirectWindowResult{}, fmt.Errorf("publish direct HLS init segment: %w", err)
			}
		}
	}
	cursor = firstPTS
	for index := 0; index <= last; index++ {
		duration := durations[index]
		if index < len(result.Fragments) {
			cursor += duration
			continue
		}
		name := fmt.Sprintf("%s%0*d_%04d%s", directSegmentPrefix, segmentNumberWidth, firstSegment, index, segmentExtension)
		if err := publishFileWithoutReplacement(filepath.Join(workDir, fmt.Sprintf("part_%06d%s", index, segmentExtension)), filepath.Join(outDir, name)); err != nil {
			return DirectWindowResult{}, fmt.Errorf("publish direct HLS segment: %w", err)
		}
		fragment := HLSFragment{
			Start:    cursor,
			Duration: duration,
			Name:     name,
			Init:     initName,
		}
		result.Fragments = append(result.Fragments, fragment)
		if onPublished != nil {
			if err := onPublished(fragment); err != nil {
				return DirectWindowResult{}, err
			}
		}
		cursor += duration
	}
	return result, nil
}

func publishDirectCoverage(
	ctx context.Context,
	workDir, outDir string,
	firstSegment int,
	start, wantedEnd float64,
	prerollBudget time.Duration,
	fmp4 bool,
	onPublished func(HLSFragment) error,
) directPublishOutcome {
	outcome := directPublishOutcome{firstPTS: math.NaN()}
	ticker := time.NewTicker(generationPollInterval)
	defer ticker.Stop()
	segmentExtension := ".ts"
	initName := ""
	if fmp4 {
		segmentExtension = ".m4s"
		initName = fmt.Sprintf("init_%0*d.mp4", segmentNumberWidth, firstSegment)
	}
	cursor := 0.0
	for {
		durations, err := readAllGeneratedDurations(filepath.Join(workDir, "window.m3u8"))
		if err == nil && len(durations) > 0 {
			if math.IsNaN(outcome.firstPTS) {
				outcome.firstPTS, err = probeFirstDirectVideoPTS(ctx, workDir, fmp4)
				if err != nil && ctx.Err() != nil {
					outcome.firstPTS = math.NaN()
					return outcome
				}
				if err == nil && (outcome.firstPTS > start+0.25 || start-outcome.firstPTS > prerollBudget.Seconds()+0.25) {
					err = fmt.Errorf("%w: nearest keyframe is %.3fs from target", ErrRemuxNeedsTranscode, start-outcome.firstPTS)
				}
				cursor = outcome.firstPTS
				if err == nil && fmp4 {
					err = publishFileWithoutReplacement(filepath.Join(workDir, "init.mp4"), filepath.Join(outDir, initName))
					if err != nil {
						err = fmt.Errorf("publish direct HLS init segment: %w", err)
					}
				}
			}
			if err != nil {
				outcome.err = fmt.Errorf("%w: %v", ErrRemuxNeedsTranscode, err)
				return outcome
			}
			for index := len(outcome.fragments); index < len(durations); index++ {
				source := filepath.Join(workDir, fmt.Sprintf("part_%06d%s", index, segmentExtension))
				if _, err := os.Stat(source); err != nil {
					if os.IsNotExist(err) {
						break
					}
					outcome.err = err
					return outcome
				}
				name := fmt.Sprintf("%s%0*d_%04d%s", directSegmentPrefix, segmentNumberWidth, firstSegment, index, segmentExtension)
				if err := publishFileWithoutReplacement(source, filepath.Join(outDir, name)); err != nil {
					outcome.err = fmt.Errorf("publish direct HLS segment: %w", err)
					return outcome
				}
				fragment := HLSFragment{Start: cursor, Duration: durations[index], Name: name, Init: initName}
				outcome.fragments = append(outcome.fragments, fragment)
				cursor += fragment.Duration
				if onPublished != nil {
					if err := onPublished(fragment); err != nil {
						outcome.err = err
						return outcome
					}
				}
				if cursor >= wantedEnd-0.05 {
					outcome.covered = true
					return outcome
				}
			}
		}
		select {
		case <-ctx.Done():
			return outcome
		case <-ticker.C:
		}
	}
}

func probeFirstDirectVideoPTS(ctx context.Context, workDir string, fmp4 bool) (float64, error) {
	segment := filepath.Join(workDir, "part_000000.ts")
	if fmp4 {
		segment = "concat:" + filepath.Join(workDir, "init.mp4") + "|" + filepath.Join(workDir, "part_000000.m4s")
	}
	return probeFirstVideoPTS(ctx, segment)
}

func GenerateVideoWindow(
	ctx context.Context,
	inputURL, outDir string,
	info domain.MediaInfo,
	selection StreamSelection,
	firstSegment, segmentCount int,
	segmentDuration time.Duration,
	onPublished func(int, float64) error,
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

func publishVideoSegments(ctx context.Context, workDir, outDir string, begin, end int, generationDone <-chan struct{}, onPublished func(int, float64) error) (int, error) {
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
			if err := publishFileWithoutReplacement(source, target); err != nil {
				return false, err
			}
			next++
			if onPublished != nil {
				if err := onPublished(next-1, duration); err != nil {
					return false, err
				}
			}
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
	return publishFileWithoutReplacement(tmp, outputPath)
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

func buildMasterPlaylist(videoList, subList string, withSubs bool, lang, assetVersion string, info domain.MediaInfo, selection StreamSelection, segmentDuration time.Duration) string {
	bandwidth := max(info.Bitrate, int64(5_500_000))
	if !CanRemuxHLS(info, selection) {
		bandwidth = compatibilityHLSBandwidth(info, selection, segmentDuration)
	}
	streamAttributes := []string{fmt.Sprintf("BANDWIDTH=%d", bandwidth)}
	if CanRemuxHLS(info, selection) {
		codec := info.VideoCodecString
		if codec != "" && selectedAudioIndex(info, selection) >= 0 {
			codec += ",mp4a.40.2"
		}
		if codec != "" {
			streamAttributes = append(streamAttributes, fmt.Sprintf("CODECS=\"%s\"", codec))
		}
		if info.HDR {
			videoRange := "PQ"
			if info.HDRFormat == "HLG" {
				videoRange = "HLG"
			}
			streamAttributes = append(streamAttributes, "VIDEO-RANGE="+videoRange)
		}
	}
	if withSubs {
		streamAttributes = append(streamAttributes, "SUBTITLES=\"subs\"")
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	if UsesFMP4(info, selection) {
		b.WriteString("#EXT-X-VERSION:7\n")
	} else {
		b.WriteString("#EXT-X-VERSION:3\n")
	}
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
	}
	b.WriteString("#EXT-X-STREAM-INF:" + strings.Join(streamAttributes, ",") + "\n")
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

func publishFileWithoutReplacement(source, target string) error {
	if err := os.Link(source, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.Remove(source)
		}
		return err
	}
	return os.Remove(source)
}
