package media

import (
	"fmt"
	"path/filepath"
)

// StreamSelection specifies the tracks selected in a generated master playlist.
type StreamSelection struct {
	AudioTrackIndex    int // -1 = default (first)
	SubtitleTrackIndex int // -1 = none
}

type argsBuilder struct {
	OutDir string
	Input  string
}

// IsBitmapSubtitle reports whether a subtitle track must be burned into video.
func IsBitmapSubtitle(codec string) bool {
	switch codec {
	case "hdmv_pgs_subtitle", "dvd_subtitle", "dvb_subtitle", "xsub":
		return true
	}
	return false
}

// buildShared emits one video rendition and every audio rendition from the input.
func (b argsBuilder) buildShared(info MediaInfo) []string {
	args := []string{"-fflags", "+genpts", "-i", b.Input, "-map", "0:v:0"}
	if info.VideoCodec == "h264" && !info.NeedFilter {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency")
		if info.NeedFilter {
			args = append(args, "-vf", "format=yuv420p")
		}
	}
	args = append(args, b.hls("video_%05d.ts", "index.m3u8")...)

	for i, track := range info.AudioTracks {
		args = append(args, "-map", fmt.Sprintf("0:a:%d", i), "-vn")
		if track.Codec == "aac" {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "aac", "-b:a", "128k", "-ac", "2")
		}
		args = append(args, b.hls(fmt.Sprintf("audio_%d_%%05d.ts", i), fmt.Sprintf("audio_%d.m3u8", i))...)
	}
	return args
}

// buildBitmap emits a video-only rendition with one bitmap subtitle track burned in.
func (b argsBuilder) buildBitmap(info MediaInfo, subtitleIndex int) []string {
	filter := fmt.Sprintf("[0:v:0][0:s:%d]overlay", subtitleIndex)
	if info.NeedFilter {
		filter += ",format=yuv420p"
	}
	filter += "[v]"
	args := []string{
		"-fflags", "+genpts",
		"-i", b.Input,
		"-filter_complex", filter,
		"-map", "[v]",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
	}
	return append(args, b.hls("video_%05d.ts", "index.m3u8")...)
}

func (b argsBuilder) hls(segmentPattern, playlist string) []string {
	return []string{
		"-f", "hls",
		"-hls_init_time", "0",
		"-hls_time", "2",
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(b.OutDir, segmentPattern),
		// Keep MPEG-TS aligned with zero-based WebVTT cue timestamps.
		"-muxdelay", "0",
		filepath.Join(b.OutDir, playlist),
	}
}
