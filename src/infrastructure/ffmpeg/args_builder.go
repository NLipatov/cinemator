package ffmpeg

import "path/filepath"

type ArgsBuilder struct {
	OutDir, Playlist string
}

func (b ArgsBuilder) Build(info SampleInfo) []string {
	// -- base flags common to every profile
	args := []string{"-fflags", "+genpts", "-i", "pipe:0"}

	// ───────────────── 1. full copy ─────────────────
	if info.VideoCodec == "h264" && info.AudioCodec == "aac" && !info.NeedFilter {
		args = append(args, "-c:v", "copy", "-c:a", "copy")
		return append(args, b.hls()...)
	}

	// ───────────────── 2. copy video, encode audio ──
	if info.VideoCodec == "h264" && !info.NeedFilter {
		args = append(args,
			"-c:v", "copy",
			"-c:a", "aac", "-b:a", "128k", "-ac", "2")
		return append(args, b.hls()...)
	}

	// ───────────────── 3. encode video, copy audio ──
	if info.AudioCodec == "aac" {
		args = append(args,
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency")
		if info.NeedFilter {
			args = append(args, "-vf", "format=yuv420p")
		}
		args = append(args, "-c:a", "copy")
		return append(args, b.hls()...)
	}

	// ───────────────── 4. full software transcode ───
	args = append(args,
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency")
	if info.NeedFilter {
		args = append(args, "-vf", "format=yuv420p")
	}
	args = append(args, "-c:a", "aac", "-b:a", "128k", "-ac", "2")
	return append(args, b.hls()...)
}

// hls returns the static HLS-muxing arguments
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
