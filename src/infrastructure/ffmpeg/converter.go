package ffmpeg

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
)

// Converter wraps “probe → decide arguments → run ffmpeg”.
type Converter struct {
	ctx       context.Context
	newReader func() io.ReadCloser // returns a fresh reader of the same input
	analyzer  SampleAnalyzer       // parses first 2 MiB via ffprobe
	builder   ArgsBuilder          // builds CLI args for ffmpeg
}

// NewConverter wires all helpers together.
func NewConverter(ctx context.Context,
	newReader func() io.ReadCloser,
	outDir, playlist string,
) *Converter {
	return &Converter{
		ctx:       ctx,
		newReader: newReader,
		analyzer:  SampleAnalyzer{},
		builder:   ArgsBuilder{OutDir: outDir, Playlist: playlist},
	}
}

// ConvertToHLS probes the stream, builds arguments once and launches ffmpeg.
func (c *Converter) ConvertToHLS() error {
	// --- 1. probe the first 2 MiB ------------------------------------
	probe := c.newReader()
	info, err := c.analyzer.Analyze(probe)
	_ = probe.Close()
	if err != nil {
		return err
	}

	// --- 2. build the final ffmpeg CLI --------------------------------
	args := c.builder.Build(info)

	// --- 3. run ffmpeg (single pass, software only) -------------------
	stream := c.newReader()
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			log.Println(closeErr)
		}
	}()

	log.Println("ffmpeg", strings.Join(args, " "))
	return runFFmpeg(c.ctx, stream, args)
}

// runFFmpeg executes ffmpeg with the provided stdin and arguments.
func runFFmpeg(ctx context.Context, in io.Reader, args []string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, os.Stdout, os.Stderr
	return cmd.Run()
}
