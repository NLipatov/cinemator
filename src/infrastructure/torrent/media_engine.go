package torrent

import (
	"context"
	"io"
	"time"

	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
)

// mediaAnalyzer is the probing boundary between torrent-backed inputs and the
// external FFprobe process.
type mediaAnalyzer interface {
	Probe(context.Context, io.Reader) (domain.MediaInfo, error)
	ProbeURL(context.Context, string) (domain.MediaInfo, error)
	ProbeTailDuration(context.Context, string, int) (float64, error)
}

// mediaPackager is the process boundary between playback orchestration and the
// external FFmpeg process. Requests describe a bounded unit of media work;
// publishing, scheduling, retries, and torrent demand remain manager concerns.
type mediaPackager interface {
	GenerateVideoWindow(
		context.Context,
		videoWindowRequest,
		func(segment int, duration float64) error,
	) (ffmpeg.VideoWindowResult, error)
	GenerateDirectWindow(
		context.Context,
		directWindowRequest,
		func(ffmpeg.HLSFragment) error,
	) (ffmpeg.DirectWindowResult, error)
	GenerateSubtitleSegment(context.Context, subtitleSegmentRequest) error
}

type videoWindowRequest struct {
	InputURL        string
	OutputDir       string
	Info            domain.MediaInfo
	Selection       ffmpeg.StreamSelection
	FirstSegment    int
	SegmentCount    int
	SegmentDuration time.Duration
}

type directWindowRequest struct {
	InputURL        string
	OutputDir       string
	Info            domain.MediaInfo
	Selection       ffmpeg.StreamSelection
	SourceSegment   int
	AssetOwner      int
	SegmentCount    int
	SegmentDuration time.Duration
	PrerollBudget   time.Duration
}

type subtitleSegmentRequest struct {
	InputURL        string
	OutputPath      string
	SubtitleTrack   int
	SegmentIndex    int
	SegmentDuration time.Duration
}

type ffmpegMediaEngine struct {
	analyzer ffmpeg.SampleAnalyzer
}

func newFFmpegMediaEngine() ffmpegMediaEngine {
	return ffmpegMediaEngine{analyzer: ffmpeg.SampleAnalyzer{}}
}

func (e ffmpegMediaEngine) Probe(ctx context.Context, input io.Reader) (domain.MediaInfo, error) {
	return e.analyzer.Analyze(ctx, input)
}

func (e ffmpegMediaEngine) ProbeURL(ctx context.Context, inputURL string) (domain.MediaInfo, error) {
	return e.analyzer.AnalyzeURL(ctx, inputURL)
}

func (e ffmpegMediaEngine) ProbeTailDuration(
	ctx context.Context,
	inputURL string,
	videoTrack int,
) (float64, error) {
	return e.analyzer.AnalyzeTailDurationURL(ctx, inputURL, videoTrack)
}

func (ffmpegMediaEngine) GenerateVideoWindow(
	ctx context.Context,
	request videoWindowRequest,
	onPublished func(int, float64) error,
) (ffmpeg.VideoWindowResult, error) {
	return ffmpeg.GenerateVideoWindow(
		ctx,
		request.InputURL,
		request.OutputDir,
		request.Info,
		request.Selection,
		request.FirstSegment,
		request.SegmentCount,
		request.SegmentDuration,
		onPublished,
	)
}

func (ffmpegMediaEngine) GenerateDirectWindow(
	ctx context.Context,
	request directWindowRequest,
	onPublished func(ffmpeg.HLSFragment) error,
) (ffmpeg.DirectWindowResult, error) {
	return ffmpeg.GenerateDirectWindow(
		ctx,
		request.InputURL,
		request.OutputDir,
		request.Info,
		request.Selection,
		request.SourceSegment,
		request.AssetOwner,
		request.SegmentCount,
		request.SegmentDuration,
		request.PrerollBudget,
		onPublished,
	)
}

func (ffmpegMediaEngine) GenerateSubtitleSegment(
	ctx context.Context,
	request subtitleSegmentRequest,
) error {
	return ffmpeg.GenerateSubtitleSegment(
		ctx,
		request.InputURL,
		request.OutputPath,
		request.SubtitleTrack,
		request.SegmentIndex,
		request.SegmentDuration,
	)
}

var (
	_ mediaAnalyzer = ffmpegMediaEngine{}
	_ mediaPackager = ffmpegMediaEngine{}
)
