package ffmpeg

import (
	"cinemator/domain"
	"cinemator/infrastructure/cli"
	"cinemator/infrastructure/hls"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Converter wraps "probe → decide arguments → run ffmpeg".
type Converter struct {
	ctx        context.Context
	inputURL   string
	analyzer   SampleAnalyzer
	builder    ArgsBuilder     // builds CLI args for ffmpeg
	selection  StreamSelection // which audio/subtitle tracks to use
	videoList  string
	subList    string
	rawSubList string
	master     string
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
	outDir, videoPlaylist, subtitlePlaylist, masterPlaylist string,
	selection StreamSelection,
) *Converter {
	return &Converter{
		ctx:        ctx,
		inputURL:   inputURL,
		analyzer:   SampleAnalyzer{},
		builder:    ArgsBuilder{OutDir: outDir, Playlist: videoPlaylist, Input: inputURL},
		selection:  selection,
		videoList:  videoPlaylist,
		subList:    subtitlePlaylist,
		rawSubList: filepath.Join(outDir, "subs.raw.m3u8"),
		master:     masterPlaylist,
	}
}

// ConvertToHLS probes the stream, builds arguments once and launches ffmpeg.
func (c *Converter) ConvertToHLS() error {
	// --- 1. probe the seekable input ----------------------------------
	info, err := c.analyzer.AnalyzeURL(c.ctx, c.inputURL)
	if err != nil {
		return err
	}
	if err := validateSelection(info, c.selection); err != nil {
		return err
	}

	// --- 2. decide subtitle strategy ---------------------------------
	hasSubtitle := c.selection.SubtitleTrackIndex >= 0 && c.selection.SubtitleTrackIndex < len(info.Subtitles)
	textSubtitle := hasSubtitle && !isBitmapSubtitle(info.Subtitles[c.selection.SubtitleTrackIndex].Codec)

	// --- 3. build the final ffmpeg CLI --------------------------------
	videoSel := c.selection
	if textSubtitle {
		videoSel.SubtitleTrackIndex = -1
	}
	args := c.builder.Build(info, videoSel)
	log.Println("ffmpeg", strings.Join(args, " "))

	// --- 4. run conversion tasks --------------------------------------
	runCtx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	runner := *c
	runner.ctx = runCtx

	tasks := []conversionTask{
		{name: "video ffmpeg", run: func() error { return runner.runFFmpeg(args) }},
	}
	if textSubtitle {
		subtitleCompleted := make(chan struct{})
		tasks = append(tasks, conversionTask{
			name: "subtitle ffmpeg",
			run: func() error {
				subArgs := runner.subtitleArgs(runner.selection.SubtitleTrackIndex)
				log.Println("ffmpeg (subtitle)", strings.Join(subArgs, " "))
				err := runner.runFFmpeg(subArgs)
				if err == nil {
					close(subtitleCompleted)
				}
				return err
			},
		}, conversionTask{
			name: "subtitle playlist",
			run: func() error {
				return runner.writeNormalizedSubtitlePlaylist(subtitleCompleted)
			},
		})
	}
	tasks = append(tasks, conversionTask{
		name: "master playlist",
		run: func() error {
			return runner.writeMasterAfterRenditionsReady(info, textSubtitle)
		},
	})

	return runConversionTasks(runCtx, cancel, tasks)
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

func runConversionTasks(ctx context.Context, cancel context.CancelFunc, tasks []conversionTask) error {
	results := make(chan conversionTaskResult, len(tasks))
	for _, task := range tasks {
		task := task
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
	_, err := cli.RunWithStdin(c.ctx, nil, "ffmpeg", args...)
	return err
}

func (c *Converter) subtitleArgs(subIdx int) []string {
	return []string{
		"-fflags", "+genpts",
		"-i", c.inputURL,
		"-map", fmt.Sprintf("0:s:%d", subIdx),
		"-c:s", "webvtt",
		"-f", "segment",
		"-segment_time", "4",
		"-segment_list", c.rawSubList,
		"-segment_list_type", "m3u8",
		"-segment_list_size", "0",
		"-segment_format", "webvtt",
		filepath.Join(c.builder.OutDir, "subs_%05d.vtt"),
	}
}

func (c *Converter) writeNormalizedSubtitlePlaylist(subtitleCompleted <-chan struct{}) error {
	preroll := filepath.Join(c.builder.OutDir, subtitlePrerollFilename)
	if err := writeFileAtomic(preroll, []byte("WEBVTT\n\n"), 0644); err != nil {
		return err
	}
	normalizer := subtitlePlaylistNormalizer{
		ctx:               c.ctx,
		rawPlaylist:       c.rawSubList,
		outPlaylist:       c.subList,
		videoList:         c.videoList,
		segmentDir:        c.builder.OutDir,
		subtitleCompleted: subtitleCompleted,
	}
	return normalizer.run()
}

func (c *Converter) writeMasterAfterRenditionsReady(info domain.MediaInfo, withSubs bool) error {
	const readinessTimeout = 20 * time.Minute
	if err := waitForPlaylistSegment(c.ctx, c.videoList, readinessTimeout); err != nil {
		return err
	}
	if withSubs {
		if err := waitForPlaylistSegment(c.ctx, c.subList, readinessTimeout); err != nil {
			return fmt.Errorf("subtitle playlist not ready: %w", err)
		}
	}

	lang := ""
	if withSubs && c.selection.SubtitleTrackIndex >= 0 && c.selection.SubtitleTrackIndex < len(info.Subtitles) {
		lang = info.Subtitles[c.selection.SubtitleTrackIndex].Language
	}
	masterData := buildMasterPlaylist(filepath.Base(c.videoList), filepath.Base(c.subList), withSubs, lang)
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
			if err == nil && hls.HasSegment(string(data)) {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("playlist segment not ready (timeout): %s", path)
			}
			time.Sleep(150 * time.Millisecond)
		}
	}
}

func buildMasterPlaylist(videoList, subList string, withSubs bool, lang string) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	if withSubs {
		attrs := []string{
			"TYPE=SUBTITLES",
			"GROUP-ID=\"subs\"",
			"NAME=\"Subtitles\"",
			"DEFAULT=YES",
			"AUTOSELECT=YES",
			"FORCED=NO",
			fmt.Sprintf("URI=\"%s\"", subList),
		}
		if lang != "" {
			attrs = append(attrs, fmt.Sprintf("LANGUAGE=\"%s\"", lang))
		}
		b.WriteString("#EXT-X-MEDIA:" + strings.Join(attrs, ",") + "\n")
		b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=2000000,SUBTITLES=\"subs\"\n")
	} else {
		b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=2000000\n")
	}
	b.WriteString(videoList + "\n")
	return b.String()
}
