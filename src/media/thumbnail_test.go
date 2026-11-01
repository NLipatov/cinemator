package media

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
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
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:2,\nvideo_00000.ts\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureThumbnail(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "thumbnail.jpg")
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	image, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil || image.Bounds().Dx() != 640 || image.Bounds().Dy() != 360 {
		t.Fatalf("thumbnail must be a 640x360 JPEG: %v", err)
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
			if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n#EXTINF:2,\n"+uri+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := EnsureThumbnail(t.Context(), dir); err == nil {
				t.Fatal("missing or unsafe segment unexpectedly succeeded")
			}
			if _, err := os.Stat(filepath.Join(dir, "thumbnail.jpg")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("thumbnail was published after failure: %v", err)
			}
		})
	}
}

func TestThumbnailCancellationDoesNotPublishAnImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n#EXTINF:2,\nvideo_00000.ts\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := EnsureThumbnail(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "thumbnail.jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("thumbnail was published after cancellation: %v", err)
	}
}
