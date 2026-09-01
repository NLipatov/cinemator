package media

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Converter creates one persisted HLS rendition set from an analyzed input.
type Converter struct {
	ctx            context.Context
	inputURL       string
	info           MediaInfo
	builder        argsBuilder
	bitmapSubtitle int
	videoList      string
	master         string
}

type conversionTask struct {
	name string
	run  func() error
}

type conversionTaskResult struct {
	name string
	err  error
}

func NewURLConverter(ctx context.Context,
	inputURL string,
	outDir, videoPlaylist, masterPlaylist string,
	info MediaInfo,
	bitmapSubtitle int,
) *Converter {
	return &Converter{
		ctx:            ctx,
		inputURL:       inputURL,
		info:           info,
		builder:        argsBuilder{OutDir: outDir, Input: inputURL},
		bitmapSubtitle: bitmapSubtitle,
		videoList:      videoPlaylist,
		master:         masterPlaylist,
	}
}

// ConvertToHLS creates either the shared renditions or one burned-in bitmap rendition.
func (c *Converter) ConvertToHLS() error {
	if c.bitmapSubtitle < -1 {
		return fmt.Errorf("invalid bitmap subtitle index: %d", c.bitmapSubtitle)
	}
	bitmap := c.bitmapSubtitle >= 0
	if bitmap {
		if c.bitmapSubtitle >= len(c.info.Subtitles) || !IsBitmapSubtitle(c.info.Subtitles[c.bitmapSubtitle].Codec) {
			return fmt.Errorf("subtitle track %d is not a bitmap subtitle", c.bitmapSubtitle)
		}
	}

	var args []string
	if bitmap {
		args = c.builder.buildBitmap(c.info, c.bitmapSubtitle)
	} else {
		args = c.builder.buildShared(c.info)
	}
	if bitmap {
		log.Printf("starting ffmpeg HLS conversion: bitmap subtitle track=%d", c.bitmapSubtitle)
	} else {
		log.Print("starting ffmpeg shared HLS conversion")
	}

	runCtx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	runner := *c
	runner.ctx = runCtx

	tasks := []conversionTask{
		{name: "video ffmpeg", run: func() error { return runner.runFFmpeg(args) }},
	}
	if !bitmap {
		textTracks := textSubtitleIndices(c.info)
		if len(textTracks) > 0 {
			preroll := filepath.Join(c.builder.OutDir, subtitlePrerollFilename)
			if err := writeFileAtomic(preroll, []byte("WEBVTT\n\n"), 0644); err != nil {
				return err
			}
		}
		for _, subtitleIndex := range textTracks {
			rawPlaylist := filepath.Join(c.builder.OutDir, fmt.Sprintf("subs_%d.raw.m3u8", subtitleIndex))
			outPlaylist := filepath.Join(c.builder.OutDir, fmt.Sprintf("subs_%d.m3u8", subtitleIndex))
			subtitleCompleted := make(chan struct{})
			tasks = append(tasks, conversionTask{
				name: fmt.Sprintf("subtitle %d ffmpeg", subtitleIndex),
				run: func() error {
					subArgs := runner.subtitleArgs(subtitleIndex, rawPlaylist)
					log.Printf("starting ffmpeg subtitle conversion: track=%d", subtitleIndex)
					err := runner.runFFmpeg(subArgs)
					if err == nil {
						close(subtitleCompleted)
					}
					return err
				},
			}, conversionTask{
				name: fmt.Sprintf("subtitle %d playlist", subtitleIndex),
				run: func() error {
					return runner.writeNormalizedSubtitlePlaylist(rawPlaylist, outPlaylist, subtitleCompleted)
				},
			})
		}
	}
	tasks = append(tasks, conversionTask{
		name: "master playlist",
		run: func() error {
			return runner.writeMasterAfterRenditionsReady()
		},
	})

	return runConversionTasks(runCtx, cancel, tasks)
}

// ValidateSelection checks track-relative indexes from the playback API.
func ValidateSelection(info MediaInfo, selection StreamSelection) error {
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

func runConversionTasks(ctx context.Context, cancel context.CancelFunc, tasks []conversionTask) error {
	results := make(chan conversionTaskResult, len(tasks))
	for _, task := range tasks {
		go func() {
			results <- conversionTaskResult{name: task.name, err: task.run()}
		}()
	}

	remaining := len(tasks)
	var firstErr error
	done := ctx.Done()
	for remaining > 0 {
		select {
		case result := <-results:
			remaining--
			if result.err != nil && firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", result.name, result.err)
				cancel()
			}
		case <-done:
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			cancel()
			done = nil
		}
	}
	return firstErr
}

func (c *Converter) runFFmpeg(args []string) error {
	_, err := runCommand(c.ctx, nil, "ffmpeg", args...)
	return err
}

func (c *Converter) subtitleArgs(subIdx int, rawPlaylist string) []string {
	return []string{
		"-fflags", "+genpts",
		"-i", c.inputURL,
		"-map", fmt.Sprintf("0:s:%d", subIdx),
		"-c:s", "webvtt",
		"-f", "segment",
		"-segment_time", "4",
		"-segment_list", rawPlaylist,
		"-segment_list_type", "m3u8",
		"-segment_list_size", "0",
		"-segment_format", "webvtt",
		filepath.Join(c.builder.OutDir, fmt.Sprintf("subs_%d_%%05d.vtt", subIdx)),
	}
}

func (c *Converter) writeNormalizedSubtitlePlaylist(rawPlaylist, outPlaylist string, subtitleCompleted <-chan struct{}) error {
	normalizer := subtitlePlaylistNormalizer{
		ctx:               c.ctx,
		rawPlaylist:       rawPlaylist,
		outPlaylist:       outPlaylist,
		videoList:         c.videoList,
		segmentDir:        c.builder.OutDir,
		subtitleCompleted: subtitleCompleted,
	}
	return normalizer.run()
}

func (c *Converter) writeMasterAfterRenditionsReady() error {
	const readinessTimeout = 20 * time.Minute
	if err := waitForPlaylistSegment(c.ctx, c.videoList, readinessTimeout); err != nil {
		return err
	}
	if c.bitmapSubtitle < 0 {
		for i := range c.info.AudioTracks {
			playlist := filepath.Join(c.builder.OutDir, fmt.Sprintf("audio_%d.m3u8", i))
			if err := waitForPlaylistSegment(c.ctx, playlist, readinessTimeout); err != nil {
				return fmt.Errorf("audio playlist %d not ready: %w", i, err)
			}
		}
		for _, i := range textSubtitleIndices(c.info) {
			playlist := filepath.Join(c.builder.OutDir, fmt.Sprintf("subs_%d.m3u8", i))
			if err := waitForPlaylistSegment(c.ctx, playlist, readinessTimeout); err != nil {
				return fmt.Errorf("subtitle playlist %d not ready: %w", i, err)
			}
		}
	}

	masterData := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=2000000\n" + filepath.Base(c.videoList) + "\n"
	return writeFileAtomic(c.master, []byte(masterData), 0644)
}

func waitForPlaylistSegment(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			data, err := os.ReadFile(path)
			if err == nil && HasSegment(string(data)) {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("playlist segment not ready (timeout): %s", path)
			}
			time.Sleep(150 * time.Millisecond)
		}
	}
}

func textSubtitleIndices(info MediaInfo) []int {
	indices := make([]int, 0, len(info.Subtitles))
	for i, track := range info.Subtitles {
		if !IsBitmapSubtitle(track.Codec) {
			indices = append(indices, i)
		}
	}
	return indices
}
