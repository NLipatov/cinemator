package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestThumbnailExtractsARealFrameAndReusesTheCache(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	segment := filepath.Join(dir, "video_00000.ts")
	_, err := runCommand(t.Context(), nil, "ffmpeg", "-v", "error", "-f", "lavfi", "-i",
		"color=c=blue:s=320x180:r=10:d=2", "-c:v", "libx264", "-threads", "1", "-f", "mpegts", segment)
	if err != nil {
		t.Fatal(err)
	}
	playlist := filepath.Join(dir, "index.m3u8")
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:2,\nvideo_00000.ts\n#EXT-X-ENDLIST\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureThumbnail(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, ThumbnailFilename)
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	image, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil || image.Bounds().Dx() != 6400 || image.Bounds().Dy() != 360 {
		t.Fatalf("thumbnail must be a 6400x360 JPEG strip: %v", err)
	}
	r, g, b, _ := image.At(320, 180).RGBA()
	if b < 40000 || r > 5000 || g > 5000 {
		t.Fatalf("thumbnail does not contain the blue source frame: %d, %d, %d", r, g, b)
	}
	before, _ := os.Stat(filename)
	if err := os.Remove(segment); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(playlist); err != nil {
		t.Fatal(err)
	}
	if err := EnsureThumbnail(t.Context(), dir); err != nil {
		t.Fatalf("cached thumbnail must not require the source: %v", err)
	}
	after, _ := os.Stat(filename)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("cached thumbnail was rewritten")
	}
}

func TestThumbnailMissingAndUnsafeSegmentsDoNotPublishAnImage(t *testing.T) {
	for _, uri := range []string{"", "../video.ts", "https://example.invalid/video.ts", "video.m3u8", "missing.ts"} {
		t.Run(uri, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n#EXTINF:2,\n"+uri+"\n#EXT-X-ENDLIST\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := EnsureThumbnail(t.Context(), dir); err == nil {
				t.Fatal("missing or unsafe segment unexpectedly succeeded")
			}
			if _, err := os.Stat(filepath.Join(dir, ThumbnailFilename)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("thumbnail was published after failure: %v", err)
			}
		})
	}
}

func TestThumbnailCancellationDoesNotPublishAnImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n#EXTINF:2,\nvideo_00000.ts\n#EXT-X-ENDLIST\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := EnsureThumbnail(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ThumbnailFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("thumbnail was published after cancellation: %v", err)
	}
}

// Color changes within one segment verify sampling by time, including seeking
// past the first few seconds instead of always decoding the opening.
func thumbnailColorVideo(t *testing.T, colors []string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	args := []string{"-v", "error", "-filter_complex_threads", "1"}
	var inputs strings.Builder
	for i, color := range colors {
		args = append(args, "-f", "lavfi", "-i", "color=c="+color+":s=320x180:r=10:d=2")
		fmt.Fprintf(&inputs, "[%d:v]", i)
	}
	args = append(args, "-filter_complex", fmt.Sprintf("%sconcat=n=%d:v=1:a=0[v]", inputs.String(), len(colors)), "-map", "[v]", "-c:v", "libx264", "-threads", "1", "-g", "10", "-f", "mpegts", filepath.Join(dir, "video.ts"))
	if _, err := runCommand(t.Context(), nil, "ffmpeg", args...); err != nil {
		t.Fatal(err)
	}
	playlist := fmt.Sprintf("#EXTM3U\n#EXTINF:%d,\nvideo.ts\n#EXT-X-ENDLIST\n", 2*len(colors))
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte(playlist), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestThumbnailSamplesEachTenthAndReplacesLegacyCache(t *testing.T) {
	for _, layout := range []string{"single segment", "uneven segments"} {
		t.Run(layout, func(t *testing.T) {
			dir := thumbnailColorVideo(t, []string{"red", "lime", "blue", "yellow", "magenta", "cyan", "white", "orange", "purple", "teal"})
			if layout == "uneven segments" {
				_, err := runCommand(t.Context(), nil, "ffmpeg", "-v", "error", "-y", "-i", filepath.Join(dir, "video.ts"), "-c:v", "copy", "-hls_time", "3", "-hls_playlist_type", "vod", "-hls_segment_filename", filepath.Join(dir, "chunk_%d.ts"), filepath.Join(dir, "index.m3u8"))
				if err != nil {
					t.Fatal(err)
				}
			}

			for _, name := range []string{"thumbnail.jpg", "thumbnail-v2.jpg"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("old preview"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := EnsureThumbnail(t.Context(), dir); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(dir, ThumbnailFilename))
			if err != nil {
				t.Fatal(err)
			}
			strip, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			want := [][3]int{{255, 0, 0}, {0, 255, 0}, {0, 0, 255}, {255, 255, 0}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255}, {255, 165, 0}, {128, 0, 128}, {0, 128, 128}}
			for part, channels := range want {
				r, g, b, _ := strip.At(part*640+320, 180).RGBA()
				got := [3]int{int(r >> 8), int(g >> 8), int(b >> 8)}
				for channel, value := range channels {
					if delta := got[channel] - value; delta < -12 || delta > 12 {
						t.Fatalf("tenth %d: RGB %v, want %v", part+1, got, channels)
					}
				}
			}

		})
	}
}

func TestThumbnailSkipsBlackTenths(t *testing.T) {
	dir := thumbnailColorVideo(t, []string{"black", "black", "blue", "black", "black"})
	if err := EnsureThumbnail(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ThumbnailFilename))
	if err != nil {
		t.Fatal(err)
	}
	strip, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for part := 0; part < 10; part++ {
		r, g, b, _ := strip.At(part*640+320, 180).RGBA()
		if b < 40000 || r > 5000 || g > 5000 {
			t.Fatalf("tenth %d contains a black frame: %d,%d,%d", part+1, r, g, b)
		}
	}
}

func TestThumbnailAllBlackHasNoCachedImage(t *testing.T) {
	dir := thumbnailColorVideo(t, []string{"black"})
	if err := EnsureThumbnail(t.Context(), dir); err == nil {
		t.Fatal("black video unexpectedly produced a preview")
	}
	if _, err := os.Stat(filepath.Join(dir, ThumbnailFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("black preview was cached: %v", err)
	}
}

func TestThumbnailWaitsForCompleteVideo(t *testing.T) {
	dir := thumbnailColorVideo(t, []string{"blue"})
	playlist := filepath.Join(dir, "index.m3u8")
	complete, err := os.ReadFile(playlist)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(playlist, bytes.ReplaceAll(complete, []byte("#EXT-X-ENDLIST\n"), nil), 0644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureThumbnail(t.Context(), dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unfinished video error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ThumbnailFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unfinished video produced preview: %v", err)
	}
	if err := os.WriteFile(playlist, complete, 0644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureThumbnail(t.Context(), dir); err != nil {
		t.Fatalf("completed video has no preview: %v", err)
	}
}
