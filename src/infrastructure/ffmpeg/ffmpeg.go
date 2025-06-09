package ffmpeg

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

type Ffmpeg struct {
	ctx              context.Context
	readCloser       io.ReadCloser
	playlist, outDir string
}

func NewFfmpeg(
	ctx context.Context,
	readCloser io.ReadCloser,
	playlist, outDir string,
) Ffmpeg {
	return Ffmpeg{
		ctx:        ctx,
		readCloser: readCloser,
		playlist:   playlist,
		outDir:     outDir,
	}
}

func (f *Ffmpeg) ConvertToHLS() error {
	defer func(readCloser io.ReadCloser) {
		_ = readCloser.Close()
	}(f.readCloser)

	cmd := exec.CommandContext(f.ctx, "ffmpeg",
		"-fflags", "+genpts",
		"-i", "pipe:0",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-vf", "format=yuv420p",
		"-c:a", "aac", "-b:a", "128k",
		"-ac", "2",
		"-movflags", "+faststart",
		"-f", "hls",
		"-hls_time", "10",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(f.outDir, "chunk_%05d.ts"),
		f.playlist,
	)
	cmd.Stdin = f.readCloser
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
