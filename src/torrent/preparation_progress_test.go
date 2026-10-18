package torrent

import (
	"crypto/sha1"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cinemator/config"
	"cinemator/media"

	torrentlib "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestListDownloadsPreparationProgress(t *testing.T) {
	root := t.TempDir()
	store, err := newDownloadStore(filepath.Join(root, "downloads"))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 40)
	if _, err := store.upsert(t.Context(), id, "magnet:?xt=urn:btih:"+id, []FileInfo{
		{Index: 0, Name: "first.mkv", Size: 100}, {Index: 1, Name: "second.mkv", Size: 200},
	}); err != nil {
		t.Fatal(err)
	}
	m := &Manager{downloads: store, cfg: config.Config{HLSPath: filepath.Join(root, "hls")}}
	check := func(stage string, index int, total int64, prepared float64) {
		t.Helper()
		downloads, err := m.ListDownloadProgress(t.Context())
		if err != nil || len(downloads) != 1 {
			t.Fatalf("ListDownloads = %+v, %v", downloads, err)
		}
		p := downloads[0].Progress
		if stage == "" {
			if p != nil {
				t.Fatalf("unexpected progress outside preparation: %+v", p)
			}
			return
		}
		if p == nil || p.Stage != stage || p.FileIndex != index || p.TotalBytes != total || p.PreparedSeconds != prepared || p.SampledAt.IsZero() {
			t.Fatalf("progress = %+v; want %s, file %d, %d bytes, %g seconds", p, stage, index, total, prepared)
		}
	}
	check("", 0, 0, 0)
	if _, err := store.beginPreparation(t.Context(), id, 0); err != nil {
		t.Fatal(err)
	}
	check("analyzing", 0, 100, 0)
	if err := store.saveMediaInfo(t.Context(), id, 0, media.MediaInfo{VideoCodec: "h264"}); err != nil {
		t.Fatal(err)
	}
	check("preparing", 0, 100, 0)
	paths := (streamKey{InfoHash: id, Index: 0, Audio: -1, Subtitle: -1}).paths(m.cfg.HLSPath)
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	playlist := "#EXTM3U\n#EXTINF:2.5,\nfirst.ts\n#EXTINF:3,\nsecond.ts\n#EXTINF:9,\n"
	if err := os.WriteFile(paths.videoPlaylist, []byte(playlist), 0644); err != nil {
		t.Fatal(err)
	}
	check("preparing", 0, 100, 5.5)
	if err := os.WriteFile(paths.masterPlaylist, []byte("#EXTM3U\nindex.m3u8\n"), 0644); err != nil {
		t.Fatal(err)
	}
	check("playable", 0, 100, 5.5)
	if _, err := store.beginPreparation(t.Context(), id, 1); err != nil {
		t.Fatal(err)
	}
	check("analyzing", 1, 200, 0)
	if err := store.failPreparation(t.Context(), id, 1, os.ErrNotExist); err != nil {
		t.Fatal(err)
	}
	check("", 0, 0, 0)
	if _, err := store.beginPreparation(t.Context(), id, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.finishPreparation(t.Context(), id, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	check("", 0, 0, 0)
}

func TestDownloadProgressOmitsDiskUsage(t *testing.T) {
	root := t.TempDir()
	store, err := newDownloadStore(filepath.Join(root, "downloads"))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("b", 40)
	if _, err := store.upsert(t.Context(), id, "magnet:?xt=urn:btih:"+id, []FileInfo{{Index: 0, Name: "movie.mkv", Size: 100}}); err != nil {
		t.Fatal(err)
	}
	m := &Manager{downloads: store, cfg: config.Config{HLSPath: filepath.Join(root, "hls")}}
	paths := (streamKey{InfoHash: id, Index: 0, Audio: -1, Subtitle: -1}).paths(m.cfg.HLSPath)
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.outDir, "video.ts"), make([]byte, 4096), 0644); err != nil {
		t.Fatal(err)
	}
	full, err := m.ListDownloads(t.Context())
	if err != nil || len(full) != 1 || full[0].DiskSize == 0 {
		t.Fatalf("full listing = %+v, %v", full, err)
	}
	light, err := m.ListDownloadProgress(t.Context())
	if err != nil || len(light) != 1 || light[0].DiskSize != 0 || light[0].ID != full[0].ID {
		t.Fatalf("light listing = %+v, %v", light, err)
	}
}

func TestListDownloadsUsesTorrentFileCompletion(t *testing.T) {
	root := t.TempDir()
	payload := []byte("verified source data")
	hash := sha1.Sum(payload)
	infoBytes, err := bencode.Marshal(metainfo.Info{Name: "movie.mkv", Length: int64(len(payload)), PieceLength: 16384, Pieces: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	meta := metainfo.MetaInfo{InfoBytes: infoBytes}
	id := meta.HashInfoBytes().HexString()
	path := filepath.Join(root, id, "movie.mkv.part")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, len(payload)), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := torrentlib.TestingConfig(t)
	cfg.DefaultStorage = storage.NewFileByInfoHash(root)
	client, err := torrentlib.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	source, _, err := client.AddTorrentSpec(torrentlib.TorrentSpecFromMetaInfo(&meta))
	if err != nil {
		t.Fatal(err)
	}
	store, err := newDownloadStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.upsert(t.Context(), id, "magnet:?xt=urn:btih:"+id, []FileInfo{{Index: 0, Name: "movie.mkv", Size: int64(len(payload))}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.beginPreparation(t.Context(), id, 0); err != nil {
		t.Fatal(err)
	}
	m := &Manager{client: client, downloads: store}
	for _, valid := range []bool{false, true} {
		if valid {
			if err := os.WriteFile(path, payload, 0644); err != nil {
				t.Fatal(err)
			}
		}
		if err := source.Piece(0).VerifyDataContext(t.Context()); err != nil {
			t.Fatal(err)
		}
		if valid {
			select {
			case <-source.Complete().On():
			case <-time.After(3 * time.Second):
				t.Fatal("verified payload did not become complete")
			}
		}
		downloads, err := m.ListDownloads(t.Context())
		if err != nil || len(downloads) != 1 {
			t.Fatalf("ListDownloads = %+v, %v", downloads, err)
		}
		want := int64(0)
		if valid {
			want = int64(len(payload))
		}
		if p := downloads[0].Progress; p == nil || p.DownloadedBytes != want || p.ReceivedBytes != 0 {
			t.Fatalf("valid=%t: progress = %+v; want %d verified bytes and no network traffic", valid, p, want)
		}
	}
}
