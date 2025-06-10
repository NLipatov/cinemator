package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	buf := make([]byte, peekSize)
	n, _ := io.ReadFull(f.readCloser, buf)
	sample := buf[:n]

	vCodec, _, aCodec, needFilter := probeSample(sample)
	copyOK := vCodec == "h264" && aCodec == "aac" && !needFilter

	// MultiReader: sample + remaining stream
	stream := io.MultiReader(bytes.NewReader(sample), f.readCloser)

	args := []string{"-fflags", "+genpts"}

	if !copyOK {
		// if transcoding is needed, try to enable hardware acceleration
		if hw, enc := detectHW(); hw != "" {
			args = append(args, "-hwaccel", hw, "-c:v", enc)
		}
	}

	args = append(args, "-i", "pipe:0")

	if copyOK {
		args = append(args, "-c:v", "copy", "-c:a", "copy")
	} else {
		args = append(args,
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-threads", "0", "-slice-max-size", "1500",
		)
		if needFilter {
			args = append(args, "-vf", "format=yuv420p")
		}
		args = append(args, "-c:a", "aac", "-b:a", "128k", "-ac", "2")
	}

	args = append(args,
		"-f", "hls",
		"-hls_init_time", "0",
		"-hls_time", "4",
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
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

func probeSample(sample []byte) (vCodec, vFmt, aCodec string, needFilter bool) {
	var out bytes.Buffer
	cmd := exec.Command("ffprobe", "-v", "error", "-of", "json", "-show_streams", "-i", "pipe:0")
	cmd.Stdin = bytes.NewReader(sample)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return
	}
	var p struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			PixFmt    string `json:"pix_fmt"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		return
	}
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			vCodec = s.CodecName
			vFmt = s.PixFmt
			if vFmt != "yuv420p" {
				needFilter = true
			}
		case "audio":
			aCodec = s.CodecName
		}
	}
	return
}

func detectHW() (hwAccel, encoder string) {
	out, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	s := string(out)
	switch {
	case strings.Contains(s, "h264_nvenc"):
		return "cuda", "h264_nvenc"
	case strings.Contains(s, "h264_qsv"):
		return "qsv", "h264_qsv"
	case strings.Contains(s, "h264_vaapi"):
		return "vaapi", "h264_vaapi"
	default:
		return "", ""
	}
}
