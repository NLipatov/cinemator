package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnsureThumbnail uses a completed local segment, never the torrent source.
// The caller serializes cache misses; publishing the JPEG is atomic.
func EnsureThumbnail(ctx context.Context, dir string) error {
	output := filepath.Join(dir, "thumbnail.jpg")
	if info, err := os.Stat(output); err == nil && info.Size() > 0 {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		return err
	}
	segments, _, err := parseMediaPlaylist(data)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return os.ErrNotExist
	}
	name := segments[0].URI
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".ts") || strings.ContainsAny(name, ":\\") {
		return fmt.Errorf("thumbnail requires a local video segment")
	}
	// Scale before sampling frames to bound memory even for a 4K source.
	image, err := runCommand(ctx, nil, "ffmpeg", "-v", "error", "-nostdin",
		"-threads", "1", "-filter_threads", "1", "-protocol_whitelist", "file,pipe",
		"-i", filepath.Join(dir, name), "-t", "8", "-an", "-sn",
		"-vf", "scale=640:360:force_original_aspect_ratio=decrease,pad=640:360:(ow-iw)/2:(oh-ih)/2,setsar=1,thumbnail=50",
		"-frames:v", "1", "-c:v", "mjpeg", "-q:v", "4", "-threads", "1", "-f", "image2pipe", "pipe:1",
	)
	if err != nil {
		return err
	}
	if len(image) == 0 {
		return fmt.Errorf("thumbnail has no image")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeFileAtomic(output, image, 0644)
}
