package ffmpeg

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
)

const peekSize = 2 << 20 // 2 MiB

// SampleInfo holds metadata from a stream sample.
type SampleInfo struct {
	VideoCodec string
	AudioCodec string
	NeedFilter bool
}

// SampleAnalyzer analyzes the first bytes of a stream.
type SampleAnalyzer struct{}

// Analyze reads peekSize bytes and returns the sample plus its metadata.
func (a SampleAnalyzer) Analyze(rc io.ReadCloser) ([]byte, SampleInfo, error) {
	buf := make([]byte, peekSize)
	n, _ := io.ReadFull(rc, buf)
	sample := buf[:n]

	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-of", "json",
		"-show_streams",
		"-i", "pipe:0",
	)
	cmd.Stdin = bytes.NewReader(sample)
	out, err := cmd.Output()
	if err != nil {
		// on error, assume we must transcode
		return sample, SampleInfo{}, nil
	}

	var meta struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			PixFmt    string `json:"pix_fmt"`
		} `json:"streams"`
	}
	_ = json.Unmarshal(out, &meta)

	info := SampleInfo{}
	for _, s := range meta.Streams {
		switch s.CodecType {
		case "video":
			info.VideoCodec = s.CodecName
			if s.PixFmt != "yuv420p" {
				info.NeedFilter = true
			}
		case "audio":
			info.AudioCodec = s.CodecName
		}
	}
	return sample, info, nil
}
