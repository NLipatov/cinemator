package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"io"
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
				{"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":3840,"height":2160,"duration":"119.5"},
			{"codec_type":"audio","codec_name":"aac","duration":"119.5"}
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
}
