package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const peekSize = 2 << 20 // 2 MiB

type Ffmpeg struct {
	ctx              context.Context
	readCloser       io.ReadCloser
	playlist, outDir string
}

func NewFfmpeg(ctx context.Context, rc io.ReadCloser, playlist, outDir string) Ffmpeg {
	return Ffmpeg{ctx: ctx, readCloser: rc, playlist: playlist, outDir: outDir}
}

func (f *Ffmpeg) ConvertToHLS() error {
	defer f.readCloser.Close()

	// — 1. Берём «кусочек» для анализа
	buf := make([]byte, peekSize)
	n, _ := io.ReadFull(f.readCloser, buf)
	sample := buf[:n]

	copyOK := probeCanCopy(sample)

	// — 2. Собираем единый поток из sample+остатка
	stream := io.MultiReader(bytes.NewReader(sample), f.readCloser)

	// — 3. Строим аргументы ffmpeg
	args := []string{"-fflags", "+genpts", "-i", "pipe:0"}
	if copyOK {
		args = append(args, "-c:v", "copy", "-c:a", "copy")
	} else {
		args = append(args,
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-vf", "format=yuv420p",
			"-c:a", "aac", "-b:a", "128k", "-ac", "2",
			"-movflags", "+faststart",
		)
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", "10",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(f.outDir, "chunk_%05d.ts"),
		f.playlist,
	)

	cmd := exec.CommandContext(f.ctx, "ffmpeg", args...)
	cmd.Stdin = stream
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// probeCanCopy = true, если в sample уже H.264 + AAC.
func probeCanCopy(sample []byte) bool {
	var out bytes.Buffer
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-of", "json",
		"-show_streams",
		"-i", "pipe:0",
	)
	cmd.Stdin = bytes.NewReader(sample)
	cmd.Stdout = &out
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return false
	}
	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out.Bytes(), &probe); err != nil {
		return false
	}
	var v, a string
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "video":
			v = s.CodecName
		case "audio":
			a = s.CodecName
		}
	}
	return v == "h264" && a == "aac"
}
