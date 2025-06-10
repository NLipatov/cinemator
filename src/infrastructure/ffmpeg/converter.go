package ffmpeg

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
)

// Converter orchestrates HLS conversion.
type Converter struct {
	ctx      context.Context
	rc       io.ReadCloser
	analyzer SampleAnalyzer
	detector HWDetector
	builder  ArgsBuilder
}

// NewConverter returns a ready-to-use Converter.
func NewConverter(ctx context.Context, rc io.ReadCloser, outDir, playlist string) *Converter {
	return &Converter{
		ctx:      ctx,
		rc:       rc,
		analyzer: SampleAnalyzer{},
		detector: HWDetector{},
		builder:  ArgsBuilder{OutDir: outDir, Playlist: playlist},
	}
}

// ConvertToHLS performs the full conversion.
func (c *Converter) ConvertToHLS() error {
	defer func(rc io.ReadCloser) {
		err := rc.Close()
		if err != nil {
			log.Println(err)
		}
	}(c.rc)

	// 1. Analyze sample
	sample, info, _ := c.analyzer.Analyze(c.rc)

	// 2. Detect HW accel
	hw := c.detector.Detect()
	c.builder.HW = hw

	// 3. Prepare stream
	stream := io.MultiReader(bytes.NewReader(sample), c.rc)

	// 4. Build args and run ffmpeg
	args := c.builder.Build(info)
	cmd := exec.CommandContext(c.ctx, "ffmpeg", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stream, os.Stdout, os.Stderr
	return cmd.Run()
}
