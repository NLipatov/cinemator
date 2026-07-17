package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

func TestCappedReaderLimitsUnderlyingReadSize(t *testing.T) {
	source := &recordingReader{reader: bytes.NewReader(make([]byte, 1024))}
	data, err := io.ReadAll(cappedReader{reader: source, max: 128})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1024 || source.largest > 128 {
		t.Fatalf("read %d bytes with largest request %d", len(data), source.largest)
	}
}

type recordingReader struct {
	reader  io.Reader
	largest int
}

func (r *recordingReader) Read(p []byte) (int, error) {
	if len(p) > r.largest {
		r.largest = len(p)
	}
	return r.reader.Read(p)
}

func TestSampleAnalyzerReturnsCanceledContextBeforeReading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (SampleAnalyzer{}).Analyze(ctx, bytes.NewReader(nil))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Analyze() error = %v, want %v", err, context.Canceled)
	}
}

func TestParseProbeOutputIncludesDurationAndBitrate(t *testing.T) {
	out := []byte(`{
		"streams":[
			{"codec_type":"video","codec_name":"h264","profile":"High","level":40,"pix_fmt":"yuv420p","bits_per_raw_sample":"8","avg_frame_rate":"24000/1001","color_primaries":"bt709","color_transfer":"bt709","color_space":"bt709","width":3840,"height":2160,"duration":"119.5"},
			{"codec_type":"audio","codec_name":"aac","profile":"LC","channels":2,"sample_rate":"48000","duration":"119.5"}
		],
		"format":{"duration":"120.25","bit_rate":"8000000"}
	}`)

	info, err := parseProbeOutput(out)
	if err != nil {
		t.Fatalf("parseProbeOutput() error = %v", err)
	}
	if info.Duration != 120.25 {
		t.Fatalf("Duration = %v, want 120.25", info.Duration)
	}
	if info.Bitrate != 8000000 {
		t.Fatalf("Bitrate = %d, want 8000000", info.Bitrate)
	}
	if info.Width != 3840 || info.Height != 2160 {
		t.Fatalf("dimensions = %dx%d", info.Width, info.Height)
	}
	if info.VideoCodecString != "avc1.640028" || info.BitDepth != 8 || info.PixelFormat != "yuv420p" || math.Abs(info.FrameRate-23.976) > 0.001 {
		t.Fatalf("browser capability metadata = %+v", info)
	}
	if info.VideoProfile != "High" || info.VideoLevel != 40 ||
		info.AudioTracks[0].Profile != "LC" || info.AudioTracks[0].Channels != 2 || info.AudioTracks[0].SampleRate != 48000 {
		t.Fatalf("codec compatibility metadata = %+v", info)
	}
	if !info.Seekable {
		t.Fatal("Seekable = false for media with a known duration")
	}
}

func TestParseProbeOutputKeepsUnknownDurationProgressive(t *testing.T) {
	out := []byte(`{
		"streams":[
			{"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p"},
			{"codec_type":"audio","codec_name":"aac"}
		],
		"format":{}
	}`)

	info, err := parseProbeOutput(out)
	if err != nil {
		t.Fatalf("parseProbeOutput() error = %v", err)
	}
	if info.Duration != 0 || info.Seekable {
		t.Fatalf("unknown-duration media = %+v, want duration 0 and progressive mode", info)
	}
	if info.VideoCodec != "h264" || len(info.AudioTracks) != 1 {
		t.Fatalf("stream metadata was lost: %+v", info)
	}
}

func TestParseProbeOutputUsesLongerVideoStreamDuration(t *testing.T) {
	out := []byte(`{
		"streams":[{"codec_type":"video","codec_name":"h264","duration_ts":12000,"time_base":"1/1000"}],
		"format":{"duration":"10"}
	}`)
	info, err := parseProbeOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Duration != 12 {
		t.Fatalf("Duration = %v, want 12", info.Duration)
	}
}

func TestParseProbeOutputDetectsHDRTransfer(t *testing.T) {
	out := []byte(`{
		"streams":[{"codec_type":"video","codec_name":"hevc","color_transfer":"smpte2084"}],
		"format":{"duration":"10"}
	}`)
	info, err := parseProbeOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if !info.HDR {
		t.Fatalf("HDR = false for PQ transfer: %+v", info)
	}
	if info.HDRFormat != "HDR10" {
		t.Fatalf("HDRFormat = %q, want HDR10", info.HDRFormat)
	}
}

func TestParseProbeOutputDetectsHDR10PlusMetadata(t *testing.T) {
	out := []byte(`{
		"streams":[{"codec_type":"video","codec_name":"hevc","color_transfer":"smpte2084","side_data_list":[{"side_data_type":"HDR10+ Metadata"}]}],
		"format":{"duration":"10"}
	}`)
	info, err := parseProbeOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if !info.HDR || info.HDRFormat != "HDR10+" {
		t.Fatalf("HDR10+ metadata = %+v", info)
	}
}

func TestParseProbeOutputBuildsAV1CapabilityString(t *testing.T) {
	out := []byte(`{
		"streams":[{"codec_type":"video","codec_name":"av1","profile":"Main","level":8,"pix_fmt":"yuv420p10le","avg_frame_rate":"60/1"}],
		"format":{"duration":"10"}
	}`)
	info, err := parseProbeOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.VideoCodecString != "av01.0.08M.10" || info.BitDepth != 10 || info.FrameRate != 60 {
		t.Fatalf("AV1 capability metadata = %+v", info)
	}
}

func TestParseTailDurationUsesLastPacketAndFormatStart(t *testing.T) {
	out := []byte(`#format: frame checksums
#tb 0: 1/1000
#stream#, dts, pts, duration, size, hash
0, 108000, 108000, 1000, 42, abc
0, 111500, 109500, 500, 42, def
0, 109000, 110000, 500, 42, ghi
`)
	duration, err := parseTailDuration(out)
	if err != nil {
		t.Fatal(err)
	}
	if duration != 110.5 {
		t.Fatalf("tail duration = %v, want 110.5", duration)
	}
}

func TestParseProbeOutputSkipsAttachedCoverArt(t *testing.T) {
	out := []byte(`{
		"streams":[
			{"codec_type":"video","codec_name":"mjpeg","width":1200,"height":1200,"disposition":{"attached_pic":1}},
			{"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1920,"height":1080,"duration":"90"}
		],
		"format":{}
	}`)
	info, err := parseProbeOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.VideoCodec != "h264" || info.VideoTrackIndex != 1 || info.Width != 1920 || info.Height != 1080 || info.Duration != 90 {
		t.Fatalf("media info = %+v", info)
	}
}

func TestParseProbeOutputHandlesRotationInterlaceAndStyledSubtitleWarning(t *testing.T) {
	out := []byte(`{
		"streams":[
			{"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1080,"height":1920,"field_order":"tt","side_data_list":[{"side_data_type":"Display Matrix","rotation":-90}]},
			{"codec_type":"subtitle","codec_name":"ass"},
			{"codec_type":"subtitle","codec_name":"ass"}
		],
		"format":{"duration":"30"}
	}`)
	info, err := parseProbeOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 1920 || info.Height != 1080 || !info.Deinterlace || !info.Rotated {
		t.Fatalf("media info = %+v", info)
	}
	if len(info.Warnings) != 1 || !strings.Contains(info.Warnings[0], "ASS/SSA") {
		t.Fatalf("warnings = %v", info.Warnings)
	}
}
