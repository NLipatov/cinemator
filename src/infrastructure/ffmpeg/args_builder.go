package ffmpeg

import (
	"cinemator/domain"
	"fmt"
	"math"
	"strings"
)

const (
	maxVideoWidth   = 1920
	maxVideoHeight  = 1080
	videoBitrate    = "4000k"
	videoMaxBitrate = "5000k"
)

// StreamSelection specifies which audio/subtitle tracks to include.
type StreamSelection struct {
	AudioTrackIndex    int // -1 = default (first)
	SubtitleTrackIndex int // -1 = none
}

// isBitmapSubtitle returns true for image-based subtitle formats
func isBitmapSubtitle(codec string) bool {
	switch codec {
	case "hdmv_pgs_subtitle", "dvd_subtitle", "dvb_subtitle", "xsub":
		return true
	}
	return false
}

func buildStreamArgs(info domain.MediaInfo, sel StreamSelection) []string {
	hasSubtitle := sel.SubtitleTrackIndex >= 0 && sel.SubtitleTrackIndex < len(info.Subtitles)
	bitmapSubtitle := hasSubtitle && isBitmapSubtitle(info.Subtitles[sel.SubtitleTrackIndex].Codec)
	var args []string
	var videoFilters []string
	if width, height, resize := boundedVideoSize(info.Width, info.Height); resize {
		videoFilters = append(videoFilters, fmt.Sprintf("scale=%d:%d", width, height))
	}
	if info.HDR {
		videoFilters = append(videoFilters,
			"zscale=t=linear:npl=100",
			"format=gbrpf32le",
			"zscale=p=bt709",
			"tonemap=tonemap=hable:desat=0",
			"zscale=t=bt709:m=bt709:r=tv",
		)
	}
	videoFilters = append(videoFilters, "format=yuv420p")

	hasAudio := len(info.AudioTracks) > 0
	audioIdx := sel.AudioTrackIndex
	if !hasAudio {
		audioIdx = -1
	} else if audioIdx < 0 || audioIdx >= len(info.AudioTracks) {
		audioIdx = 0
	}

	// -- handle subtitles: bitmap vs text require different approaches
	if bitmapSubtitle {
		// Tone-map the source before overlaying SDR bitmap subtitles.
		filter := fmt.Sprintf("[0:v]%s[base];[base][0:s:%d]overlay,format=yuv420p[v]",
			strings.Join(videoFilters, ","), sel.SubtitleTrackIndex)
		args = append(args,
			"-filter_complex", filter,
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

	// Each requested HLS window starts at an exact virtual segment boundary.
	// Re-encoding discards frames before that boundary and creates independent
	// keyframes; stream-copy would start at an earlier source keyframe instead.
	args = append(args,
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-b:v", videoBitrate,
		"-maxrate", videoMaxBitrate,
		"-bufsize", "10000k",
	)
	if !bitmapSubtitle && len(videoFilters) > 0 {
		args = append(args, "-vf", strings.Join(videoFilters, ","))
	}

	// Audio is encoded as well so its timestamps begin at the same exact boundary.
	if hasAudio {
		args = append(args, "-c:a", "aac", "-b:a", "128k", "-ac", "2")
	}

	return args
}

func boundedVideoSize(width, height int) (int, int, bool) {
	if width <= 0 || height <= 0 || width <= maxVideoWidth && height <= maxVideoHeight {
		return width, height, false
	}
	scale := math.Min(float64(maxVideoWidth)/float64(width), float64(maxVideoHeight)/float64(height))
	targetWidth := max(2, int(float64(width)*scale)/2*2)
	targetHeight := max(2, int(float64(height)*scale)/2*2)
	return targetWidth, targetHeight, true
}
