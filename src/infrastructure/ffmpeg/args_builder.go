package ffmpeg

import "path/filepath"
import "fmt"

type ArgsBuilder struct {
	OutDir, Playlist string
}

func (b ArgsBuilder) Build(info SampleInfo, audioIdx int) []string {
	// -- base flags common to every profile
	args := []string{"-fflags", "+genpts", "-i", "pipe:0"}

	// map video
	args = append(args, "-map", "0:v:0")

	// map audio (if any audio tracks presented)
	if len(info.AudioTracks) > 0 && audioIdx >= 0 && audioIdx < len(info.AudioTracks) {
		args = append(args, "-map", fmt.Sprintf("0:a:%d", audioIdx))
		track := info.AudioTracks[audioIdx]
		if track.NeedsTranscode {
			args = append(args, "-c:a", "aac", "-b:a", "128k", "-ac", "2")
		} else {
			args = append(args, "-c:a", "copy")
		}
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
