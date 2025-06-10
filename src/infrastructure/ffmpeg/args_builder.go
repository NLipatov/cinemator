package ffmpeg

import "path/filepath"

type ArgsBuilder struct {
	OutDir   string
	Playlist string
	HW       HWAccel
}

func (b ArgsBuilder) Build(info SampleInfo) (primary, fallback []string) {
	copyOK := info.VideoCodec == "h264" &&
		info.AudioCodec == "aac" &&
		!info.NeedFilter

	baseArgs := []string{"-fflags", "+genpts", "-i", "pipe:0"}

	hlsArgs := []string{
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
		args := append(baseArgs, "-c:v", "copy", "-c:a", "copy")
		return append(args, hlsArgs...), append(args, hlsArgs...)
	}

	// primary (hardware)
	if b.HW.Name != "" && b.HW.Decoder != "" && b.HW.Encoder != "" {
		primary = append(baseArgs,
			"-hwaccel", b.HW.Name, "-c:v", b.HW.Decoder,
			"-c:v", b.HW.Encoder,
		)
	} else {
		primary = append(baseArgs,
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		)
	}

	// fallback (always software)
	fallback = append(baseArgs,
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
	)

	if info.NeedFilter {
		primary = append(primary, "-vf", "format=yuv420p")
		fallback = append(fallback, "-vf", "format=yuv420p")
	}

	audioArgs := []string{"-c:a", "aac", "-b:a", "128k", "-ac", "2"}

	primary = append(primary, audioArgs...)
	fallback = append(fallback, audioArgs...)

	primary = append(primary, hlsArgs...)
	fallback = append(fallback, hlsArgs...)

	return primary, fallback
}
