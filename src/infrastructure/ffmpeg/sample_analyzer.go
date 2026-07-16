package ffmpeg

import (
	"bytes"
	"cinemator/domain"
	"cinemator/infrastructure/cli"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

const (
	initialProbeBytes = 1 << 20
	probeStepBytes    = 4 << 20
	maxProbeBytes     = 16 << 20
	probeReadBytes    = 256 << 10
)

type SampleAnalyzer struct{}

func (SampleAnalyzer) Analyze(ctx context.Context, r io.Reader) (domain.MediaInfo, error) {
	data := make([]byte, 0, maxProbeBytes)
	target := initialProbeBytes
	var lastErr error

	for {
		if err := ctx.Err(); err != nil {
			return domain.MediaInfo{}, err
		}
		prevLen := len(data)
		need := target - len(data)
		if need > 0 {
			if need > maxProbeBytes-len(data) {
				need = maxProbeBytes - len(data)
			}
			tmp := make([]byte, need)
			n, readErr := io.ReadFull(cappedReader{reader: r, max: probeReadBytes}, tmp)
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

		info, err := probeSample(ctx, data)
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

type cappedReader struct {
	reader io.Reader
	max    int
}

func (r cappedReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		p = p[:r.max]
	}
	return r.reader.Read(p)
}

func (SampleAnalyzer) AnalyzeURL(ctx context.Context, sourceURL string) (domain.MediaInfo, error) {
	out, err := cli.RunWithStdin(ctx, nil,
		"ffprobe", "-v", "error",
		"-probesize", strconv.Itoa(maxProbeBytes),
		"-analyzeduration", "10000000",
		"-read_intervals", "%+5",
		"-of", "json", "-show_streams", "-show_format", "-i", sourceURL,
	)
	if err != nil {
		return domain.MediaInfo{}, err
	}
	return parseProbeOutput(out)
}

func probeSample(ctx context.Context, sample []byte) (domain.MediaInfo, error) {
	out, err := cli.RunWithStdin(ctx, bytes.NewReader(sample),
		"ffprobe", "-v", "error",
		"-of", "json", "-show_streams", "-show_format", "-i", "pipe:0",
	)
	if err != nil {
		return domain.MediaInfo{}, err
	}
	return parseProbeOutput(out)
}

func parseProbeOutput(out []byte) (domain.MediaInfo, error) {
	var meta struct {
		Streams []struct {
			Index         int    `json:"index"`
			CodecType     string `json:"codec_type"`
			CodecName     string `json:"codec_name"`
			PixFmt        string `json:"pix_fmt"`
			ColorTransfer string `json:"color_transfer"`
			Width         int    `json:"width"`
			Height        int    `json:"height"`
			Duration      string `json:"duration"`
			DurationTS    int64  `json:"duration_ts"`
			TimeBase      string `json:"time_base"`
			Tags          struct {
				Language string `json:"language"`
				Title    string `json:"title"`
			} `json:"tags"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
			Bitrate  string `json:"bit_rate"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		return domain.MediaInfo{}, err
	}

	var info domain.MediaInfo
	info.Duration, _ = strconv.ParseFloat(meta.Format.Duration, 64)
	info.Bitrate, _ = strconv.ParseInt(meta.Format.Bitrate, 10, 64)
	audioIdx := 0
	subIdx := 0
	for _, s := range meta.Streams {
		switch s.CodecType {
		case "video":
			if info.VideoCodec == "" {
				info.VideoCodec = s.CodecName
				info.Width = s.Width
				info.Height = s.Height
				info.HDR = s.ColorTransfer == "smpte2084" || s.ColorTransfer == "arib-std-b67"
			}
			streamDuration, _ := strconv.ParseFloat(s.Duration, 64)
			if streamDuration <= 0 && s.DurationTS > 0 {
				streamDuration = float64(s.DurationTS) * parseTimeBase(s.TimeBase)
			}
			if streamDuration > info.Duration {
				info.Duration = streamDuration
			}
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
	info.Seekable = info.Duration > 0
	return info, nil
}

func parseTimeBase(value string) float64 {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return 0
	}
	numerator, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	denominator, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || denominator == 0 {
		return 0
	}
	return numerator / denominator
}
