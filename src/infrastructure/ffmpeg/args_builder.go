package ffmpeg

import (
	"log"
	"path/filepath"
)

type ArgsBuilder struct {
	OutDir   string
	Playlist string
	HW       HWAccel
}

func (b ArgsBuilder) Build(info SampleInfo) (primary, fallback []string) {
	copyOK := info.VideoCodec == "h264" && info.AudioCodec == "aac" && !info.NeedFilter

	common := []string{"-fflags", "+genpts"}
	hls := []string{
		"-f", "hls",
		"-hls_init_time", "0",
		"-hls_time", "4",
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(b.OutDir, "chunk_%05d.ts"),
		b.Playlist,
	}

	if copyOK {
		args := append(common, "-i", "pipe:0", "-c:v", "copy", "-c:a", "copy")
		args = append(args, hls...)
		return args, args
	}

	primary = append(primary, common...)

	switch b.HW.Name {
	case "vaapi":
		primary = append(primary,
			"-init_hw_device", "vaapi=va:/dev/dri/renderD128",
			"-hwaccel", "vaapi",
			"-hwaccel_output_format", "vaapi")
	case "qsv":
		primary = append(primary,
			"-init_hw_device", "qsv=hw:/dev/dri/renderD128",
			"-hwaccel", "qsv")
	case "":
	default:
		primary = append(primary, "-hwaccel", b.HW.Name)
	}
	if b.HW.Decoder != "" {
		primary = append(primary, "-c:v", b.HW.Decoder)
	}
	primary = append(primary, "-i", "pipe:0")
	if b.HW.Encoder != "" {
		primary = append(primary, "-c:v", b.HW.Encoder)
		primary = append(primary, b.presetFor(b.HW.Encoder)...)
	} else {
		primary = append(primary, "-c:v", "libx264")
		primary = append(primary, b.presetFor("libx264")...)
	}
	if info.NeedFilter {
		primary = append(primary, "-vf", "format=yuv420p")
	}
	primary = append(primary, "-c:a", "aac", "-b:a", "128k", "-ac", "2")
	primary = append(primary, hls...)

	fallback = append(fallback, common...)                         // ① сначала добавляем срез
	fallback = append(fallback, "-i", "pipe:0", "-c:v", "libx264") // ② потом обычные аргументы
	fallback = append(fallback, b.presetFor("libx264")...)
	if info.NeedFilter {
		fallback = append(fallback, "-vf", "format=yuv420p")
	}
	fallback = append(fallback, "-c:a", "aac", "-b:a", "128k", "-ac", "2")
	fallback = append(fallback, hls...)

	fallback = append(fallback, b.presetFor("libx264")...)
	if info.NeedFilter {
		fallback = append(fallback, "-vf", "format=yuv420p")
	}
	fallback = append(fallback, "-c:a", "aac", "-b:a", "128k", "-ac", "2")
	fallback = append(fallback, hls...)

	return primary, fallback
}

func (b ArgsBuilder) presetFor(enc string) []string {
	switch enc {
	case "libx264":
		return []string{"-preset", "ultrafast", "-tune", "zerolatency"}
	case "h264_nvenc":
		return []string{"-preset", "fast"}
	case "h264_qsv":
		return []string{"-preset", "veryfast"}
	case "h264_vaapi":
		return []string{"-profile:v", "main", "-qp", "20"}
	case "h264_v4l2m2m":
		return []string{"-profile:v", "high"}
	default:
		log.Printf("no preset for encoder: %s", enc)
		return nil
	}
}
