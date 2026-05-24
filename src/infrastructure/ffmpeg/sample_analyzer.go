package ffmpeg

import (
	"cinemator/domain"
	"cinemator/infrastructure/cli"
	"context"
	"encoding/json"
)

type SampleAnalyzer struct{}

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
