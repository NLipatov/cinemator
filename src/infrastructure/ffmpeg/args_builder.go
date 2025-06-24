package ffmpeg

import "path/filepath"
import "fmt"

type ArgsBuilder struct {
	OutDir, Playlist string
}

func (b ArgsBuilder) Build(info SampleInfo) []string {
	// -- base flags common to every profile
	args := []string{"-fflags", "+genpts", "-i", "pipe:0"}

	// map video
	args = append(args, "-map", "0:v:0")

	// map every audiotrack
	for i := range info.AudioTracks {
		args = append(args, "-map", fmt.Sprintf("0:a:%d", i))
	}

	// Video: if h264 and filter not needed - copy, in other case — transcode
	if info.VideoCodec == "h264" && !info.NeedFilter {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency")
		if info.NeedFilter {
			args = append(args, "-vf", "format=yuv420p")
		}
	}

	// Audio: if AAC - copy, in other case -transcode
	for i, track := range info.AudioTracks {
		if track.NeedsTranscode {
			args = append(args, fmt.Sprintf("-c:a:%d", i), "aac", fmt.Sprintf("-b:a:%d", i), "128k", fmt.Sprintf("-ac:%d", i), "2")
		} else {
			args = append(args, fmt.Sprintf("-c:a:%d", i), "copy")
		}
	}

	// HLS muxing
	return append(args, b.hls()...)
}

func (b ArgsBuilder) hls() []string {
	return []string{
		"-f", "hls",
		"-hls_init_time", "0",
		"-hls_time", "4",
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(b.OutDir, "chunk_%05d.ts"),
		b.Playlist,
	}
}
