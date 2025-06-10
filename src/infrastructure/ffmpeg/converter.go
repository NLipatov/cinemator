package ffmpeg

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
)

type Converter struct {
	ctx       context.Context
	newReader func() io.ReadCloser
	analyzer  SampleAnalyzer
	detector  HWDetector
	builder   ArgsBuilder
}

func NewConverter(ctx context.Context, newReader func() io.ReadCloser, outDir, playlist string) *Converter {
	return &Converter{
		ctx:       ctx,
		newReader: newReader,
		analyzer:  SampleAnalyzer{},
		detector:  HWDetector{},
		builder:   ArgsBuilder{OutDir: outDir, Playlist: playlist},
	}
}

func (c *Converter) ConvertToHLS() error {
	rc1 := c.newReader()
	sample, info, _ := c.analyzer.Analyze(rc1)
	rc1.Close()

	hw := c.detector.Detect()
	c.builder.HW = hw
	primary, fallback := c.builder.Build(info)

	rc := c.newReader()
	defer rc.Close()
	stream := io.MultiReader(bytes.NewReader(sample), rc)

	if err := runFFmpeg(c.ctx, stream, primary); err != nil {
		log.Printf("Primary encoding failed (%v). Trying software fallback.", err)

		rcFallback := c.newReader()
		defer rcFallback.Close()
		streamFallback := io.MultiReader(bytes.NewReader(sample), rcFallback)

		return runFFmpeg(c.ctx, streamFallback, fallback)
	}

	return nil
}

func runFFmpeg(ctx context.Context, stream io.Reader, args []string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stream, os.Stdout, os.Stderr
	return cmd.Run()
}
