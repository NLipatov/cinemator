package ffmpeg

import (
	"cinemator/domain"
	"fmt"
	"path/filepath"
	"strings"
)

// StreamSelection specifies which audio/subtitle tracks to include.
type StreamSelection struct {
	AudioTrackIndex    int // -1 or out of range = default (first)
	SubtitleTrackIndex int // -1 = none
}

type ArgsBuilder struct {
	OutDir, Playlist string
	Input            string
}

// isBitmapSubtitle returns true for image-based subtitle formats
func isBitmapSubtitle(codec string) bool {
	switch codec {
	case "hdmv_pgs_subtitle", "dvd_subtitle", "dvb_subtitle", "xsub":
		return true
	}
	return false
}

func (b ArgsBuilder) Build(info domain.MediaInfo, sel StreamSelection) []string {
	hasSubtitle := sel.SubtitleTrackIndex >= 0 && sel.SubtitleTrackIndex < len(info.Subtitles)

	args := []string{"-fflags", "+genpts", "-i", b.Input}

	hasAudio := len(info.AudioTracks) > 0
	audioIdx := sel.AudioTrackIndex
	if !hasAudio {
		audioIdx = -1
	} else if audioIdx < 0 || audioIdx >= len(info.AudioTracks) {
		audioIdx = 0
	}

	// -- determine audio codec for selected track
	audioCodec := ""
	if hasAudio {
		audioCodec = info.AudioTracks[audioIdx].Codec
	}

	// -- video/audio encoding decisions
	needVideoEncode := info.VideoCodec != "h264" || info.NeedFilter || hasSubtitle
	needAudioEncode := audioCodec != "aac"

	// -- handle subtitles: bitmap vs text require different approaches
	if hasSubtitle && isBitmapSubtitle(info.Subtitles[sel.SubtitleTrackIndex].Codec) {
		// Bitmap subtitles: use filter_complex with overlay
		args = append(args,
			"-filter_complex", fmt.Sprintf("[0:v][0:s:%d]overlay[v]", sel.SubtitleTrackIndex),
			"-map", "[v]",
		)
	} else if hasSubtitle {
		// Text subtitles: emit video here and write WebVTT in the subtitle pass
		args = append(args,
			"-map", "0:v:0",
		)
	} else {
		// No subtitles: simple mapping
		args = append(args,
			"-map", "0:v:0",
		)
	}
	if hasAudio {
		args = append(args, "-map", fmt.Sprintf("0:a:%d", audioIdx))
	}

	if needVideoEncode {
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency")
		var vfParts []string
		if info.NeedFilter {
			vfParts = append(vfParts, "format=yuv420p")
		}
		if len(vfParts) > 0 {
			args = append(args, "-vf", strings.Join(vfParts, ","))
		}
	} else {
		args = append(args, "-c:v", "copy")
	}

	// -- audio encoding
	if hasAudio {
		if needAudioEncode {
			args = append(args, "-c:a", "aac", "-b:a", "128k", "-ac", "2")
		} else {
			args = append(args, "-c:a", "copy")
		}
	}

	return append(args, b.hls()...)
}

// hls returns the HLS-muxing arguments.
func (b ArgsBuilder) hls() []string {
	return []string{
		"-f", "hls",
		"-hls_init_time", "0",
		"-hls_time", "2",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(b.OutDir, "chunk_%05d.ts"),
		b.Playlist,
	}
}
