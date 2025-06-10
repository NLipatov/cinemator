package ffmpeg

import (
	"path/filepath"
)

// ArgsBuilder constructs ffmpeg arguments based on sample info and hw accel.
type ArgsBuilder struct {
	OutDir   string
	Playlist string
	HW       HWAccel
}

// Build returns a slice of ffmpeg args for HLS conversion.
func (b ArgsBuilder) Build(info SampleInfo) []string {
	copyOK := info.VideoCodec == "h264" &&
		info.AudioCodec == "aac" &&
		!info.NeedFilter

	args := []string{"-fflags", "+genpts"}
	if !copyOK && b.HW.Name != "" {
		args = append(args, "-hwaccel", b.HW.Name)
		if b.HW.Decoder != "" {
			args = append(args, "-c:v", b.HW.Decoder)
		}
	}
	args = append(args, "-i", "pipe:0")

	if copyOK {
		args = append(args, "-c:v", "copy", "-c:a", "copy")
	} else {
		if b.HW.Encoder != "" {
			args = append(args, "-c:v", b.HW.Encoder)
		} else {
			args = append(args,
				"-c:v", "libx264",
				"-preset", "ultrafast",
				"-tune", "zerolatency",
				"-threads", "0",
				"-slice-max-size", "1500",
			)
		}
		if info.NeedFilter {
			args = append(args, "-vf", "format=yuv420p")
		}
		args = append(args, "-c:a", "aac", "-b:a", "128k", "-ac", "2")
	}

	// HLS options
	args = append(args,
		"-f", "hls",
		"-hls_init_time", "0",
		"-hls_time", "4",
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(b.OutDir, "chunk_%05d.ts"),
		b.Playlist,
	)
	return args
}
