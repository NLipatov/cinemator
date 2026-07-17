package ffmpeg

import (
	"bytes"
	"cinemator/domain"
	"cinemator/infrastructure/cli"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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
	data := make([]byte, 0, initialProbeBytes)
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
			start := len(data)
			data = append(data, make([]byte, need)...)
			n, readErr := io.ReadFull(cappedReader{reader: r, max: probeReadBytes}, data[start:])
			data = data[:start+n]
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

// AnalyzeTailDurationURL seeks from EOF and derives duration from actual video
// packet timestamps. Container headers occasionally misreport duration,
// which would otherwise truncate the tail or advertise positions past EOF.
// Framehash gives us packet timestamps on stdout without decoding the video.
func (SampleAnalyzer) AnalyzeTailDurationURL(ctx context.Context, sourceURL string, videoTrackIndex int) (float64, error) {
	out, err := cli.RunWithStdin(ctx, nil,
		"ffmpeg", "-v", "error",
		"-sseof", "-30",
		"-copyts", "-start_at_zero",
		"-i", sourceURL,
		"-map", fmt.Sprintf("0:v:%d", max(0, videoTrackIndex)), "-an", "-sn", "-dn",
		"-c:v", "copy", "-hash", "md5", "-f", "framehash", "-",
	)
	if err != nil {
		return 0, err
	}
	return parseTailDuration(out)
}

func parseTailDuration(out []byte) (float64, error) {
	timeBase := 0.0
	lastEnd := int64(0)
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#tb 0:") {
			timeBase = parseTimeBase(strings.TrimSpace(strings.TrimPrefix(line, "#tb 0:")))
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 4 {
			continue
		}
		pts, ptsErr := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		packetDuration, durationErr := strconv.ParseInt(strings.TrimSpace(fields[3]), 10, 64)
		if ptsErr != nil || durationErr != nil {
			continue
		}
		lastEnd = max(lastEnd, pts+max(0, packetDuration))
	}
	duration := float64(lastEnd) * timeBase
	if duration <= 0 {
		return 0, fmt.Errorf("ffmpeg returned no usable tail timestamps")
	}
	return duration, nil
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
			Index            int    `json:"index"`
			CodecType        string `json:"codec_type"`
			CodecName        string `json:"codec_name"`
			Profile          string `json:"profile"`
			Level            int    `json:"level"`
			Channels         int    `json:"channels"`
			SampleRate       string `json:"sample_rate"`
			PixFmt           string `json:"pix_fmt"`
			BitsPerRawSample string `json:"bits_per_raw_sample"`
			ColorPrimaries   string `json:"color_primaries"`
			ColorTransfer    string `json:"color_transfer"`
			ColorSpace       string `json:"color_space"`
			FieldOrder       string `json:"field_order"`
			Width            int    `json:"width"`
			Height           int    `json:"height"`
			AverageFrameRate string `json:"avg_frame_rate"`
			Duration         string `json:"duration"`
			DurationTS       int64  `json:"duration_ts"`
			TimeBase         string `json:"time_base"`
			Tags             struct {
				Language string `json:"language"`
				Title    string `json:"title"`
				Rotate   string `json:"rotate"`
			} `json:"tags"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
			SideData []struct {
				Type     string `json:"side_data_type"`
				Rotation int    `json:"rotation"`
			} `json:"side_data_list"`
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
	if info.Duration <= 0 || math.IsNaN(info.Duration) || math.IsInf(info.Duration, 0) {
		info.Duration = 0
	}
	info.Bitrate, _ = strconv.ParseInt(meta.Format.Bitrate, 10, 64)
	audioIdx := 0
	subIdx := 0
	videoIdx := 0
	warnedStyledSubtitles := false
	for _, s := range meta.Streams {
		switch s.CodecType {
		case "video":
			if s.Disposition.AttachedPic != 0 || info.VideoCodec != "" {
				videoIdx++
				continue
			}
			info.VideoCodec = s.CodecName
			info.VideoProfile = s.Profile
			info.VideoLevel = s.Level
			info.VideoCodecString = videoCodecString(s.CodecName, s.Profile, s.Level, videoBitDepth(s.PixFmt, s.BitsPerRawSample))
			info.VideoTrackIndex = videoIdx
			info.Width = s.Width
			info.Height = s.Height
			info.FrameRate = parseTimeBase(s.AverageFrameRate)
			info.PixelFormat = s.PixFmt
			info.BitDepth = videoBitDepth(s.PixFmt, s.BitsPerRawSample)
			info.ColorPrimaries = s.ColorPrimaries
			info.ColorTransfer = s.ColorTransfer
			info.ColorSpace = s.ColorSpace
			info.Rotated = displayRotation(s.Tags.Rotate, s.SideData)
			if info.Rotated {
				info.Width, info.Height = info.Height, info.Width
			}
			switch s.ColorTransfer {
			case "smpte2084":
				info.HDR = true
				info.HDRFormat = "HDR10"
			case "arib-std-b67":
				info.HDR = true
				info.HDRFormat = "HLG"
			}
			info.Deinterlace = s.FieldOrder != "" && s.FieldOrder != "unknown" && s.FieldOrder != "progressive"
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
			for _, sideData := range s.SideData {
				typeName := strings.ToLower(sideData.Type)
				if strings.Contains(typeName, "dovi") {
					info.HDR = true
					info.HDRFormat = "Dolby Vision"
					info.DolbyVision = true
					info.Warnings = append(info.Warnings, "Dolby Vision playback depends on client support; unsupported clients use the SDR fallback.")
					break
				}
				if strings.Contains(typeName, "hdr10+") || strings.Contains(typeName, "dynamic hdr plus") {
					info.HDR = true
					info.HDRFormat = "HDR10+"
				}
			}
			videoIdx++
		case "audio":
			sampleRate, _ := strconv.Atoi(s.SampleRate)
			info.AudioTracks = append(info.AudioTracks, domain.AudioTrack{
				Index:      audioIdx,
				Codec:      s.CodecName,
				Profile:    s.Profile,
				Channels:   s.Channels,
				SampleRate: sampleRate,
				Language:   s.Tags.Language,
				Title:      s.Tags.Title,
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
			if !warnedStyledSubtitles && (s.CodecName == "ass" || s.CodecName == "ssa") {
				info.Warnings = append(info.Warnings, "Styled ASS/SSA subtitles are converted to plain WebVTT, so advanced positioning and effects may be simplified.")
				warnedStyledSubtitles = true
			}
		}
	}
	info.Seekable = info.Duration > 0
	return info, nil
}

func displayRotation(tag string, sideData []struct {
	Type     string `json:"side_data_type"`
	Rotation int    `json:"rotation"`
}) bool {
	rotation, _ := strconv.Atoi(tag)
	for _, data := range sideData {
		if data.Rotation != 0 {
			rotation = data.Rotation
			break
		}
	}
	normalized := ((rotation % 360) + 360) % 360
	return normalized == 90 || normalized == 270
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

func videoBitDepth(pixelFormat, raw string) int {
	if value, err := strconv.Atoi(raw); err == nil && value > 0 {
		return value
	}
	for _, depth := range []int{16, 14, 12, 10, 9} {
		if strings.Contains(pixelFormat, strconv.Itoa(depth)) {
			return depth
		}
	}
	if pixelFormat != "" {
		return 8
	}
	return 0
}

func videoCodecString(codec, profile string, level, bitDepth int) string {
	switch codec {
	case "h264":
		prefix := "6400"
		switch strings.ToLower(profile) {
		case "baseline":
			prefix = "4200"
		case "constrained baseline":
			prefix = "42e0"
		case "main":
			prefix = "4d00"
		}
		if level > 0 {
			return fmt.Sprintf("avc1.%s%02x", prefix, level)
		}
		return "avc1"
	case "hevc":
		profileID := 1
		compatibility := 6
		if strings.Contains(strings.ToLower(profile), "10") {
			profileID = 2
			compatibility = 4
		}
		if level > 0 {
			return fmt.Sprintf("hvc1.%d.%d.L%d.B0", profileID, compatibility, level)
		}
		return "hvc1"
	case "av1":
		profileID := 0
		switch strings.ToLower(profile) {
		case "high":
			profileID = 1
		case "professional":
			profileID = 2
		}
		if level >= 0 && bitDepth > 0 {
			return fmt.Sprintf("av01.%d.%02dM.%02d", profileID, level, bitDepth)
		}
		return "av01"
	default:
		return ""
	}
}
