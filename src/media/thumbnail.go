package media

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
)

// Version the cache and URL together to replace old first-segment previews.
const ThumbnailFilename = "thumbnail-v2.jpg"

// EnsureThumbnail caches a horizontal strip of five frames, one from each fifth
// of a completed local video. It never reads or starts the torrent source.
// The caller serializes cache misses; publishing the JPEG is atomic.
func EnsureThumbnail(ctx context.Context, dir string) error {
	output := filepath.Join(dir, ThumbnailFilename)
	if info, err := os.Stat(output); err == nil && info.Size() > 0 {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		return err
	}
	segments, ended, err := parseMediaPlaylist(data)
	if err != nil {
		return err
	}
	if !ended || len(segments) == 0 {
		return os.ErrNotExist
	}
	var duration float64
	for _, segment := range segments {
		name := segment.URI
		if filepath.Base(name) != name || !strings.HasSuffix(name, ".ts") || strings.ContainsAny(name, ":\\") {
			return fmt.Errorf("thumbnail requires local video segments")
		}
		duration += segment.Duration
	}
	if duration <= 0 {
		return fmt.Errorf("thumbnail requires a video duration")
	}
	var frames [5]image.Image
	var fallback image.Image
	for part := range frames {
		// Retry a black frame at another random point in the same fifth. Avoid
		// exact boundaries and limit decoding independently of the video's length.
		for attempt := 0; attempt < 3; attempt++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			target := (float64(part) + 0.1 + 0.8*rand.Float64()) * duration / 5
			remaining := (float64(part+1)*duration/5 - target)
			for _, segment := range segments {
				if target >= segment.Duration {
					target -= segment.Duration
					continue
				}
				frame, err := thumbnailFrame(ctx, filepath.Join(dir, segment.URI), target,
					math.Min(2, math.Min(remaining, segment.Duration-target)))
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err == nil {
					frames[part] = frame
					fallback = frame
				}
				break
			}
			if frames[part] != nil {
				break
			}
		}
	}
	if fallback == nil {
		return fmt.Errorf("thumbnail has no non-black frame")
	}
	strip := image.NewRGBA(image.Rect(0, 0, 5*640, 360))
	for part, frame := range frames {
		if frame == nil {
			// A fully black fifth uses another sampled scene rather than a black tile.
			frame = fallback
		}
		draw.Draw(strip, image.Rect(part*640, 0, (part+1)*640, 360), frame, image.Point{}, draw.Src)
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, strip, &jpeg.Options{Quality: 85}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeFileAtomic(output, encoded.Bytes(), 0644)
}

func thumbnailFrame(ctx context.Context, filename string, offset, duration float64) (image.Image, error) {
	// Scale before sampling to bound memory for 4K sources. Reject near-black
	// frames before padding so letterboxing does not affect the decision.
	// Decode from the segment's keyframe: input seeking in MPEG-TS may jump
	// past the requested point (or past EOF for a segment with only one keyframe).
	filter := fmt.Sprintf("trim=start=%.6f:duration=%.6f,setpts=PTS-STARTPTS,", offset, duration) +
		"scale=640:360:force_original_aspect_ratio=decrease:out_range=tv,format=yuv420p,blackframe=amount=0:threshold=32,metadata=select:key=lavfi.blackframe.pblack:value=98:function=less,pad=640:360:(ow-iw)/2:(oh-ih)/2,setsar=1,thumbnail=50,format=yuvj420p"
	data, err := runCommand(ctx, nil, "ffmpeg", "-v", "error", "-nostdin",
		"-threads", "1", "-filter_threads", "1", "-protocol_whitelist", "file,pipe",
		"-t", fmt.Sprintf("%.6f", offset+duration), "-i", filename, "-an", "-sn", "-vf", filter,
		"-frames:v", "1", "-c:v", "mjpeg", "-q:v", "4", "-threads", "1", "-f", "image2pipe", "pipe:1",
	)
	if err != nil {
		return nil, err
	}
	return jpeg.Decode(bytes.NewReader(data))
}
