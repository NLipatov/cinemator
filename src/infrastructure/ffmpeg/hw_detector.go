package ffmpeg

import (
	"os/exec"
	"strings"
)

// HWAccel describes a hardware acceleration option.
type HWAccel struct {
	Name    string // e.g., "cuda"
	Encoder string // e.g., "h264_nvenc"
}

// HWDetector inspects ffmpeg for available hw encoders.
type HWDetector struct{}

// Detect returns the first matching hardware encoder.
func (d HWDetector) Detect() HWAccel {
	out, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	s := string(out)
	switch {
	case strings.Contains(s, "h264_nvenc"):
		return HWAccel{"cuda", "h264_nvenc"}
	case strings.Contains(s, "h264_qsv"):
		return HWAccel{"qsv", "h264_qsv"}
	case strings.Contains(s, "h264_vaapi"):
		return HWAccel{"vaapi", "h264_vaapi"}
	default:
		return HWAccel{"", ""}
	}
}
