package ffmpeg

import (
	"os/exec"
	"strings"
)

type HWAccel struct {
	Name    string
	Decoder string
	Encoder string
}

type HWDetector struct{}

func (d HWDetector) Detect() HWAccel {
	out, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	s := string(out)
	switch {
	case strings.Contains(s, "h264_cuvid") && strings.Contains(s, "h264_nvenc"):
		return HWAccel{"cuda", "h264_cuvid", "h264_nvenc"}
	case strings.Contains(s, "h264_qsv"):
		return HWAccel{"qsv", "h264_qsv", "h264_qsv"}
	case strings.Contains(s, "h264_vaapi"):
		return HWAccel{"vaapi", "", "h264_vaapi"}
	default:
		return HWAccel{}
	}
}
