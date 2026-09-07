package torrent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cinemator/config"

	torrentlib "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestHLSPreparationWithLocalMedia(t *testing.T) {
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s is required for the media integration test", binary)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	subtitles := filepath.Join(root, "subtitles.srt")
	if err := os.WriteFile(subtitles, []byte("1\n00:00:01,000 --> 00:00:03,000\nHello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "movie.mkv")
	command := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=160x90:r=10:d=6",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6", "-f", "lavfi", "-i", "sine=frequency=660:duration=6",
		"-i", subtitles, "-map", "0:v", "-map", "1:a", "-map", "2:a", "-map", "3:s",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "20", "-c:a", "aac", "-c:s", "srt", input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate media: %v\n%s", err, output)
	}
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(input); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	meta := metainfo.MetaInfo{InfoBytes: infoBytes}
	id := meta.HashInfoBytes().HexString()
	downloadRoot := filepath.Join(root, "downloads")
	payloadDir := filepath.Join(downloadRoot, id)
	if err := os.MkdirAll(payloadDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(input, filepath.Join(payloadDir, info.Name)); err != nil {
		t.Fatal(err)
	}
	clientConfig := torrentlib.TestingConfig(t)
	clientConfig.DefaultStorage = storage.NewFileByInfoHash(downloadRoot)
	client, err := torrentlib.NewClient(clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if _, _, err := client.AddTorrentSpec(torrentlib.TorrentSpecFromMetaInfo(&meta)); err != nil {
		t.Fatal(err)
	}
	sources, err := newRangeServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sources.srv.Close() })
	store, err := newDownloadStore(downloadRoot)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		client: client, sources: sources, downloads: store,
		active: make(map[streamKey]*streamInfo),
		cfg:    config.Config{HLSPath: filepath.Join(root, "hls"), DownloadPath: downloadRoot},
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.DeleteDownload(cleanupCtx, id); err != nil && !errors.Is(err, ErrDownloadNotFound) {
			t.Error(err)
		}
	})
	magnet := "magnet:?xt=urn:btih:" + id
	if _, err := m.GetTorrentFiles(ctx, magnet); err != nil {
		t.Fatal(err)
	}
	playlist, err := m.PrepareHlsStream(ctx, magnet, 0, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	master, err := os.ReadFile(playlist)
	if err != nil || !strings.Contains(string(master), "audio_1.m3u8") || !strings.Contains(string(master), "subs_0.m3u8") {
		t.Fatalf("selected master = %s, %v", master, err)
	}
	key := streamKey{InfoHash: id, Index: 0, Audio: -1, Subtitle: -1}
	stream, err := m.getStream(ctx, key)
	if err != nil || stream == nil {
		t.Fatalf("stream = %v, %v", stream, err)
	}
	if err := waitForDone(ctx, stream.runDone); err != nil {
		t.Fatal(err)
	}
	downloads, err := m.ListDownloads(ctx)
	if err != nil || len(downloads) != 1 || downloads[0].Status != DownloadStatusReady {
		t.Fatalf("completed downloads = %#v, %v", downloads, err)
	}
	// Wait for torrent readers to close and the temporary source to be removed.
	if err := waitForDone(ctx, m.cleanupTransientPayload(id, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(payloadDir, info.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary payload was retained: %v", err)
	}
	if err := m.cleanup(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PrepareHlsStream(ctx, magnet, 0, 0, -1); err != nil {
		t.Fatalf("reopen completed HLS without its torrent source: %v", err)
	}
}
