package media

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
)

const (
	initialProbeBytes = 1 << 20
	probeStepBytes    = 4 << 20
	maxProbeBytes     = 16 << 20
)

func Probe(ctx context.Context, r io.Reader) (MediaInfo, error) {
	data := make([]byte, 0, maxProbeBytes)
	target := initialProbeBytes
	var lastErr error

	for {
		if err := ctx.Err(); err != nil {
			return MediaInfo{}, err
		}
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
					return MediaInfo{}, readErr
				}
				if n == 0 {
					if lastErr != nil {
						return MediaInfo{}, lastErr
					}
					return MediaInfo{}, readErr
				}
			}
		}

		info, err := probeSample(ctx, data)
		if err == nil {
			return info, nil
		}
		lastErr = err

		if len(data) >= maxProbeBytes {
			return MediaInfo{}, lastErr
		}
		if len(data) == prevLen {
			return MediaInfo{}, lastErr
		}
		target += probeStepBytes
		if target > maxProbeBytes {
			target = maxProbeBytes
		}
	}
}

func ProbeURL(ctx context.Context, sourceURL string) (MediaInfo, error) {
	out, err := runCommand(ctx, nil,
		"ffprobe", "-v", "error",
		"-of", "json", "-show_streams", "-i", sourceURL,
	)
	if err != nil {
		return MediaInfo{}, err
	}
	return parseProbeOutput(out)
}

func probeSample(ctx context.Context, sample []byte) (MediaInfo, error) {
	out, err := runCommand(ctx, bytes.NewReader(sample),
		"ffprobe", "-v", "error",
		"-of", "json", "-show_streams", "-i", "pipe:0",
	)
	if err != nil {
		return MediaInfo{}, err
	}
	return parseProbeOutput(out)
}

func parseProbeOutput(out []byte) (MediaInfo, error) {
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
		return MediaInfo{}, err
	}

	var info MediaInfo
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
			info.AudioTracks = append(info.AudioTracks, AudioTrack{
				Index:    audioIdx,
				Codec:    s.CodecName,
				Language: s.Tags.Language,
				Title:    s.Tags.Title,
			})
			audioIdx++
		case "subtitle":
			info.Subtitles = append(info.Subtitles, SubtitleTrack{
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
