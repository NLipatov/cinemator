package ffmpeg

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
)

// first 2 MiB of the stream are enough for ffprobe to parse container headers
const peekSize = 2 << 20 // 2 MiB

type AudioTrack struct {
	Index          int
	Codec          string
	NeedsTranscode bool // true if not AAC
}

type SampleInfo struct {
	VideoCodec  string
	AudioTracks []AudioTrack
	NeedFilter  bool // true when pixel-format ≠ yuv420p
}

type SampleAnalyzer struct{}

func (SampleAnalyzer) Analyze(r io.Reader) (SampleInfo, error) {
	buf := make([]byte, peekSize)
	n, _ := io.ReadFull(r, buf)
	sample := buf[:n]

	cmd := exec.Command(
		"ffprobe", "-v", "error",
		"-of", "json", "-show_streams", "-i", "pipe:0",
	)
	cmd.Stdin = bytes.NewReader(sample)
	out, err := cmd.Output()
	if err != nil {
		return SampleInfo{}, err
	}

	var meta struct {
		Streams []struct {
			Index     int    `json:"index"`
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
			needsTranscode := s.CodecName != "aac"
			info.AudioTracks = append(info.AudioTracks, AudioTrack{
				Index:          s.Index,
				Codec:          s.CodecName,
				NeedsTranscode: needsTranscode,
			})
		}
	}
	return info, nil
}
