package ffmpeg

import (
	"bytes"
	"os"
	"os/exec"
)

type HWAccel struct {
	Name    string
	Decoder string
	Encoder string
}

type HWDetector struct{}

func (HWDetector) Detect() HWAccel {
	// NVIDIA NVENC ------------------------------------------------------
	if _, err := os.Stat("/dev/nvidia0"); err == nil {
		return HWAccel{"cuda", "h264_cuvid", "h264_nvenc"}
	}

	// /dev/dri/renderD128 → Intel / AMD / Broadcom
	if _, err := os.Stat("/dev/dri/renderD128"); err == nil {
		out, _ := exec.Command("sh", "-c",
			`lspci -nn | grep -m1 '\[03..:.*\]'`).Output()

		switch {
		case bytes.Contains(out, []byte("Intel")):
			return HWAccel{"qsv", "h264_qsv", "h264_qsv"}
		case bytes.Contains(out, []byte("AMD")), bytes.Contains(out, []byte("ATI")):
			return HWAccel{"vaapi", "", "h264_vaapi"} // AMD VCN
		case bytes.Contains(out, []byte("Broadcom")):
			return HWAccel{"v4l2", "h264_v4l2m2m", "h264_v4l2m2m"} // RPi or others.
		}
	}

	// soft
	return HWAccel{}
}
