package ffmpeg

import (
	"bytes"
	"cinemator/domain"
	"cinemator/infrastructure/cli"
	"context"
	"encoding/json"
	"io"
)

// first 2 MiB of the stream are enough for ffprobe to parse container headers
const peekSize = 2 << 20 // 2 MiB

type SampleAnalyzer struct{}

// Analyze reads up to peekSize bytes, feeds them to ffprobe and
// returns detected codecs, audio tracks, subtitles and whether a yuv420p conversion is required.
func (SampleAnalyzer) Analyze(r io.Reader) (domain.MediaInfo, error) {
	// --- 1. grab a small probe chunk ---------------------------------
	buf := make([]byte, peekSize)
	n, _ := io.ReadFull(r, buf) // ignore error: short read is fine
	sample := buf[:n]

	out, err := cli.RunWithStdin(context.Background(), bytes.NewReader(sample),
		"ffprobe", "-v", "error",
		"-of", "json", "-show_streams", "-i", "pipe:0",
	)
	if err != nil {
		return domain.MediaInfo{}, err
	}

	// --- 2. parse json ------------------------------------------------
	var meta struct {
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			PixFmt    string `json:"pix_fmt"`
			Tags      struct {
				Language string `json:"language"`
				Title    string `json:"title"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		return domain.MediaInfo{}, err
	}

	// --- 3. build result ---------------------------------------------
	var info domain.MediaInfo
	audioIdx := 0
	subIdx := 0
	for _, s := range meta.Streams {
		switch s.CodecType {
		case "video":
			info.VideoCodec = s.CodecName
			if s.PixFmt != "yuv420p" && s.PixFmt != "" {
				info.NeedFilter = true
			}
		case "audio":
			info.AudioTracks = append(info.AudioTracks, domain.AudioTrack{
				Index:    audioIdx,
				Codec:    s.CodecName,
				Language: s.Tags.Language,
				Title:    s.Tags.Title,
			})
			audioIdx++
		case "subtitle":
			info.Subtitles = append(info.Subtitles, domain.SubtitleTrack{
				Index:    subIdx,
				Codec:    s.CodecName,
				Language: s.Tags.Language,
				Title:    s.Tags.Title,
			})
			subIdx++
		}
	}
	return info, nil
}
