package ffmpeg

import (
	"cinemator/domain"
	"cinemator/infrastructure/cli"
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
	ctx       context.Context
	inputURL  string
	analyzer  SampleAnalyzer
	builder   ArgsBuilder     // builds CLI args for ffmpeg
	selection StreamSelection // which audio/subtitle tracks to use
	videoList string
	subList   string
	master    string
}

func NewURLConverter(ctx context.Context,
	inputURL string,
	outDir, videoPlaylist, subtitlePlaylist, masterPlaylist string,
	selection StreamSelection,
) *Converter {
	return &Converter{
		ctx:       ctx,
		inputURL:  inputURL,
		analyzer:  SampleAnalyzer{},
		builder:   ArgsBuilder{OutDir: outDir, Playlist: videoPlaylist, Input: inputURL},
		selection: selection,
		videoList: videoPlaylist,
		subList:   subtitlePlaylist,
		master:    masterPlaylist,
	}
}

// ConvertToHLS probes the stream, builds arguments once and launches ffmpeg.
func (c *Converter) ConvertToHLS() error {
	// --- 1. probe the seekable input ----------------------------------
	info, err := c.analyzer.AnalyzeURL(c.ctx, c.inputURL)
	if err != nil {
		return err
	}

	// --- 2. decide subtitle strategy ---------------------------------
	hasSubtitle := c.selection.SubtitleTrackIndex >= 0 && c.selection.SubtitleTrackIndex < len(info.Subtitles)
	textSubtitle := hasSubtitle && !isBitmapSubtitle(info.Subtitles[c.selection.SubtitleTrackIndex].Codec)
	if textSubtitle {
		_ = c.writeEmptySubtitlePlaylist()
	}

	// --- 3. build the final ffmpeg CLI --------------------------------
	videoSel := c.selection
	if textSubtitle {
		videoSel.SubtitleTrackIndex = -1
	}
	args := c.builder.Build(info, videoSel)
	log.Println("ffmpeg", strings.Join(args, " "))

	// --- 4. run ffmpeg ------------------------------------------------
	videoErrCh := make(chan error, 1)
	go func() {
		err := c.runFFmpeg(args)
		videoErrCh <- err
	}()

	var subtitleErrCh chan error
	if textSubtitle {
		subtitleErrCh = make(chan error, 1)
		go func() {
			subArgs := c.subtitleArgs(c.selection.SubtitleTrackIndex)
			log.Println("ffmpeg (subtitle)", strings.Join(subArgs, " "))
			err := c.runFFmpeg(subArgs)
			subtitleErrCh <- err
		}()
	}

	masterReady := make(chan error, 1)
	go func() { masterReady <- c.writeMasterAfterVideoReady(info, textSubtitle) }()

	subtitleDone := subtitleErrCh == nil
	videoDone := false
	masterDone := false

	for {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case err := <-videoErrCh:
			videoDone = true
			if err != nil {
				return err
			}
			if videoDone && subtitleDone && masterDone {
				return nil
			}
		case err := <-masterReady:
			masterDone = true
			if err != nil {
				return err
			}
			if videoDone && subtitleDone && masterDone {
				return nil
			}
		case err := <-subtitleErrCh:
			subtitleDone = true
			if err != nil {
				log.Printf("Subtitle stream error: %v", err)
				return err
			}
			if videoDone && subtitleDone && masterDone {
				return nil
			}
		}
	}
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
		"-segment_list", c.subList,
		"-segment_list_type", "m3u8",
		"-segment_list_size", "0",
		"-segment_format", "webvtt",
		filepath.Join(c.builder.OutDir, "subs_%05d.vtt"),
	}
}

func (c *Converter) writeMasterAfterVideoReady(info domain.MediaInfo, withSubs bool) error {
	if err := waitForFile(c.ctx, c.videoList, 20*time.Minute); err != nil {
		return err
	}

	lang := ""
	if withSubs && c.selection.SubtitleTrackIndex >= 0 && c.selection.SubtitleTrackIndex < len(info.Subtitles) {
		lang = info.Subtitles[c.selection.SubtitleTrackIndex].Language
	}
	masterData := buildMasterPlaylist(filepath.Base(c.videoList), filepath.Base(c.subList), withSubs, lang)
	return os.WriteFile(c.master, []byte(masterData), 0644)
}

func (c *Converter) writeEmptySubtitlePlaylist() error {
	data := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n"
	return os.WriteFile(c.subList, []byte(data), 0644)
}

func waitForFile(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if _, err := os.Stat(path); err == nil {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("playlist not ready (timeout): %s", path)
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
