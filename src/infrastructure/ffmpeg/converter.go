package ffmpeg

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
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
	// ---------- анализ ----------------------------------------------------------------
	rcProbe := c.newReader()
	_, info, _ := c.analyzer.Analyze(rcProbe)
	rcProbe.Close()

	hw := c.detector.Detect()
	if !c.smokeTest(hw) {
		hw = HWAccel{}
	}
	fmt.Println("HW accel selected:", hw.Name, hw.Decoder, hw.Encoder)

	c.builder.HW = hw

	primary, fallback := c.builder.Build(info)

	rc := c.newReader()
	defer rc.Close()
	log.Println("Trying primary ffmpeg:", strings.Join(primary, " "))
	if err := runFFmpeg(c.ctx, rc, primary); err != nil {
		log.Printf("Primary failed (%v). Falling back to software.", err)

		rcFB := c.newReader()
		defer rcFB.Close()
		return runFFmpeg(c.ctx, rcFB, fallback)
	}
	return nil
}

func runFFmpeg(ctx context.Context, in io.Reader, args []string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, os.Stdout, os.Stderr
	return cmd.Run()
}

func (c *Converter) smokeTest(hw HWAccel) bool {
	if hw.Name == "" {
		return false
	}
	args := []string{"-v", "error", "-f", "lavfi", "-i", "nullsrc", "-t", "0.1"}
	switch hw.Name {
	case "vaapi":
		args = append(args,
			"-init_hw_device", "vaapi=va:/dev/dri/renderD128",
			"-hwaccel", "vaapi", "-c:v", hw.Encoder)
	case "qsv":
		args = append(args,
			"-init_hw_device", "qsv=hw:/dev/dri/renderD128",
			"-hwaccel", "qsv", "-c:v", hw.Encoder)
	case "v4l2":
		args = append(args, "-c:v", hw.Encoder)
	default:
		args = append(args, "-hwaccel", hw.Name, "-c:v", hw.Encoder)
	}
	args = append(args, "-f", "null", "-")
	return exec.Command("ffmpeg", args...).Run() == nil
}
