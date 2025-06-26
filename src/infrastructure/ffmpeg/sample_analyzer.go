package ffmpeg

import (
	"bytes"
	"cinemator/domain"
	"encoding/json"
	"io"
	"os/exec"
)

// first 2 MiB of the stream are enough for ffprobe to parse container headers
const peekSize = 2 << 20 // 2 MiB

type SampleInfo struct {
	VideoCodec  string
	AudioTracks []domain.AudioInfo
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
			Tags      struct {
				Title    string `json:"title"`
				Language string `json:"language"`
			} `json:"tags"`
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
			info.AudioTracks = append(info.AudioTracks, domain.AudioInfo{
				Index: s.Index,
				Codec: s.CodecName,
				Tag: domain.AudioTag{
					Title:    s.Tags.Title,
					Language: s.Tags.Language,
				},
				NeedsTranscode: s.CodecName != "aac",
			})
		}
	}
	return info, nil
}
