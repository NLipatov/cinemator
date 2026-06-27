package ffmpeg

import (
	"bytes"
	"cinemator/domain"
	"cinemator/infrastructure/cli"
	"context"
	"encoding/json"
	"io"
)

const (
	initialProbeBytes = 1 << 20
	probeStepBytes    = 4 << 20
	maxProbeBytes     = 16 << 20
)

type SampleAnalyzer struct{}

func (SampleAnalyzer) Analyze(r io.Reader) (domain.MediaInfo, error) {
	data := make([]byte, 0, maxProbeBytes)
	target := initialProbeBytes
	var lastErr error

	for {
		prevLen := len(data)
		need := target - len(data)
		if need > 0 {
			if need > maxProbeBytes-len(data) {
				need = maxProbeBytes - len(data)
			}
			tmp := make([]byte, need)
			n, readErr := io.ReadFull(r, tmp)
			data = append(data, tmp[:n]...)
			if readErr != nil {
				if readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
					return domain.MediaInfo{}, readErr
				}
				if n == 0 {
					if lastErr != nil {
						return domain.MediaInfo{}, lastErr
					}
					return domain.MediaInfo{}, readErr
				}
			}
		}

		info, err := probeSample(data)
		if err == nil {
			return info, nil
		}
		lastErr = err

		if len(data) >= maxProbeBytes {
			return domain.MediaInfo{}, lastErr
		}
		if len(data) == prevLen {
			return domain.MediaInfo{}, lastErr
		}
		target += probeStepBytes
		if target > maxProbeBytes {
			target = maxProbeBytes
		}
	}
}

func (SampleAnalyzer) AnalyzeURL(ctx context.Context, sourceURL string) (domain.MediaInfo, error) {
	out, err := cli.RunWithStdin(ctx, nil,
		"ffprobe", "-v", "error",
		"-of", "json", "-show_streams", "-i", sourceURL,
	)
	if err != nil {
		return domain.MediaInfo{}, err
	}
	return parseProbeOutput(out)
}

func probeSample(sample []byte) (domain.MediaInfo, error) {
	out, err := cli.RunWithStdin(context.Background(), bytes.NewReader(sample),
		"ffprobe", "-v", "error",
		"-of", "json", "-show_streams", "-i", "pipe:0",
	)
	if err != nil {
		return domain.MediaInfo{}, err
	}
	return parseProbeOutput(out)
}

func parseProbeOutput(out []byte) (domain.MediaInfo, error) {
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
