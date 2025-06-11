package ffmpeg

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
)

// first 2 MiB of the stream are enough for ffprobe to parse container headers
const peekSize = 2 << 20 // 2 MiB

type SampleInfo struct {
	VideoCodec string
	AudioCodec string
	NeedFilter bool // true when pixel-format ≠ yuv420p
}

type SampleAnalyzer struct{}

// Analyze reads up to peekSize bytes, feeds them to ffprobe and
// returns detected codecs + whether a yuv420p conversion is required.
func (SampleAnalyzer) Analyze(r io.Reader) (SampleInfo, error) {
	// --- 1. grab a small probe chunk ---------------------------------
	buf := make([]byte, peekSize)
	n, _ := io.ReadFull(r, buf) // ignore error: short read is fine
	sample := buf[:n]

	// --- 2. run ffprobe on the chunk ---------------------------------
	cmd := exec.Command(
		"ffprobe", "-v", "error",
		"-of", "json", "-show_streams", "-i", "pipe:0",
	)
	cmd.Stdin = bytes.NewReader(sample)
	out, err := cmd.Output()
	if err != nil {
		return SampleInfo{}, err
	}

	// --- 3. parse json ------------------------------------------------
	var meta struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			PixFmt    string `json:"pix_fmt"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		return SampleInfo{}, err
	}

	// --- 4. build result ---------------------------------------------
	var info SampleInfo
	for _, s := range meta.Streams {
		switch s.CodecType {
		case "video":
			info.VideoCodec = s.CodecName
			if s.PixFmt != "yuv420p" && s.PixFmt != "" {
				info.NeedFilter = true
			}
		case "audio":
			info.AudioCodec = s.CodecName
		}
	}
	return info, nil
}
