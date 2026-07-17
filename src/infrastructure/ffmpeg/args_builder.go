package ffmpeg

import (
	"cinemator/domain"
	"fmt"
	"strings"
)

// StreamSelection specifies which audio/subtitle tracks to include.
type StreamSelection struct {
	AudioTrackIndex    int // -1 = default (first)
	SubtitleTrackIndex int // -1 = none
	ForceTranscode     bool
}

// CanRemuxHLS reports whether the selected video can be sent to browsers
// unchanged. Unknown-duration inputs keep the progressive transcoding path
// because they cannot expose a stable sparse VOD timeline.
func CanRemuxHLS(info domain.MediaInfo, sel StreamSelection) bool {
	if sel.ForceTranscode || info.Duration <= 0 || info.Deinterlace || info.Rotated || info.DolbyVision {
		return false
	}
	if sel.SubtitleTrackIndex >= 0 &&
		sel.SubtitleTrackIndex < len(info.Subtitles) &&
		isBitmapSubtitle(info.Subtitles[sel.SubtitleTrackIndex].Codec) {
		return false
	}
	switch info.VideoCodec {
	case "h264":
		return !info.HDR && copyableH264Profile(info.VideoProfile, info.VideoLevel) && copyablePixelFormat(info)
	case "hevc":
		return copyableHEVCProfile(info.VideoProfile, info.VideoLevel) && copyablePixelFormat(info)
	case "av1":
		return copyableAV1Profile(info.VideoProfile, info.VideoLevel) && copyablePixelFormat(info)
	default:
		return false
	}
}

func UsesFMP4(info domain.MediaInfo, sel StreamSelection) bool {
	return CanRemuxHLS(info, sel) && (info.VideoCodec == "hevc" || info.VideoCodec == "av1")
}

func CopiesAudio(info domain.MediaInfo, sel StreamSelection) bool {
	index := selectedAudioIndex(info, sel)
	if index < 0 {
		return true
	}
	track := info.AudioTracks[index]
	return track.Codec == "aac" &&
		(track.Profile == "" || strings.EqualFold(track.Profile, "LC")) &&
		(track.Channels == 0 || track.Channels <= 2) &&
		(track.SampleRate == 0 || track.SampleRate <= 48000)
}

func copyableH264Profile(profile string, level int) bool {
	switch strings.ToLower(profile) {
	case "", "baseline", "constrained baseline", "main", "high":
		return level <= 62
	default:
		return false
	}
}

func copyableHEVCProfile(profile string, level int) bool {
	profile = strings.ToLower(profile)
	return (profile == "" || profile == "main" || profile == "main 10") && level <= 186
}

func copyableAV1Profile(profile string, level int) bool {
	profile = strings.ToLower(profile)
	return (profile == "" || profile == "main") && level <= 18
}

func copyablePixelFormat(info domain.MediaInfo) bool {
	if info.PixelFormat == "" {
		return !info.NeedFilter
	}
	switch info.VideoCodec {
	case "h264":
		return info.PixelFormat == "yuv420p"
	case "hevc", "av1":
		return info.PixelFormat == "yuv420p" || info.PixelFormat == "yuv420p10le"
	default:
		return false
	}
}

func HLSMode(info domain.MediaInfo, sel StreamSelection) string {
	if !CanRemuxHLS(info, sel) {
		return "transcode"
	}
	if CopiesAudio(info, sel) {
		return "direct"
	}
	return "hybrid"
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
	videoMap := fmt.Sprintf("0:v:%d", max(0, info.VideoTrackIndex))
	var args []string
	var videoFilters []string
	if info.Deinterlace {
		videoFilters = append(videoFilters, "bwdif=mode=send_frame:parity=auto:deint=interlaced")
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

	audioIdx := selectedAudioIndex(info, sel)
	hasAudio := audioIdx >= 0

	// -- handle subtitles: bitmap vs text require different approaches
	if bitmapSubtitle {
		// Tone-map the source before overlaying SDR bitmap subtitles.
		filter := fmt.Sprintf("[%s]%s[base];[base][0:s:%d]overlay,format=yuv420p[v]",
			videoMap, strings.Join(videoFilters, ","), sel.SubtitleTrackIndex)
		args = append(args,
			"-filter_complex", filter,
			"-map", "[v]",
		)
	} else if hasSubtitle {
		// Text subtitles: emit video here and write WebVTT in the subtitle pass
		args = append(args,
			"-map", videoMap,
		)
	} else {
		// No subtitles: simple mapping
		args = append(args,
			"-map", videoMap,
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
		"-crf", "18",
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

func buildRemuxStreamArgs(info domain.MediaInfo, sel StreamSelection) []string {
	args := []string{"-map", fmt.Sprintf("0:v:%d", max(0, info.VideoTrackIndex)), "-c:v", "copy"}
	if info.VideoCodec == "hevc" {
		args = append(args, "-tag:v", "hvc1")
	} else if info.VideoCodec == "av1" {
		args = append(args, "-tag:v", "av01")
	}
	audioIdx := selectedAudioIndex(info, sel)
	if audioIdx < 0 {
		return args
	}
	args = append(args, "-map", fmt.Sprintf("0:a:%d", audioIdx))
	if CopiesAudio(info, sel) {
		return append(args, "-c:a", "copy")
	}
	return append(args, "-c:a", "aac", "-b:a", "128k", "-ac", "2")
}

func selectedAudioIndex(info domain.MediaInfo, sel StreamSelection) int {
	if len(info.AudioTracks) == 0 {
		return -1
	}
	if sel.AudioTrackIndex < 0 || sel.AudioTrackIndex >= len(info.AudioTracks) {
		return 0
	}
	return sel.AudioTrackIndex
}
