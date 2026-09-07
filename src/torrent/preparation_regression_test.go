package torrent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cinemator/config"

	torrentlib "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

func TestStartHLSPreparationKeepsSameFileBitmapRendition(t *testing.T) {
	root := t.TempDir()
	store, err := newDownloadStore(filepath.Join(root, "downloads"))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 40)
	magnet := "magnet:?xt=urn:btih:" + id
	if _, err := store.upsert(context.Background(), id, magnet, []FileInfo{{Index: 0, Name: "movie.mkv"}}); err != nil {
		t.Fatal(err)
	}
	base := streamKey{InfoHash: id, Index: 0, Audio: -1, Subtitle: -1}
	bitmap := base
	bitmap.Subtitle = 0
	hlsRoot := filepath.Join(root, "hls")
	paths := bitmap.paths(hlsRoot)
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	segment := filepath.Join(paths.outDir, "video_000.ts")
	if err := os.WriteFile(segment, []byte("already playable segment"), 0644); err != nil {
		t.Fatal(err)
	}
	bitmapCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Manager{
		active: map[streamKey]*streamInfo{
			base:   {paths: base.paths(hlsRoot)},
			bitmap: {paths: paths, cancel: cancel},
		},
		streamOps: make(map[streamKey]chan struct{}),
		downloads: store,
		cfg:       config.Config{HLSPath: hlsRoot},
	}
	if err := m.StartHLSPreparation(context.Background(), magnet, 0); err != nil {
		t.Fatal(err)
	}
	if bitmapCtx.Err() != nil {
		t.Error("re-selecting the same file canceled its running bitmap subtitle rendition")
	}
	if _, err := os.Stat(segment); err != nil {
		t.Errorf("re-selecting the same file removed its playable bitmap segment: %v", err)
	}
}

func TestQueuedPreparationKeepsTorrentSource(t *testing.T) {
	client, err := torrentlib.NewClient(torrentlib.TestingConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	id := strings.Repeat("b", 40)
	source, err := client.AddMagnet("magnet:?xt=urn:btih:" + id)
	if err != nil {
		t.Fatal(err)
	}
	key := streamKey{InfoHash: id, Index: 0, Audio: -1, Subtitle: -1}
	m := &Manager{client: client, active: map[streamKey]*streamInfo{key: {}}}
	if err := waitForDone(t.Context(), m.cleanupTransientPayload(id, source)); err != nil {
		t.Fatal(err)
	}
	if current, exists := client.Torrent(metainfo.NewHashFromHex(id)); !exists || current != source {
		t.Fatal("source was dropped before the queued preparation could retain it")
	}
}

func TestAllocatedFileSizeOfSparseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparse")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	blocks, ok := fileBlocks(info)
	if !ok || blocks != 0 {
		t.Skip("filesystem did not create a zero-block sparse file")
	}
	if got := allocatedFileSize(info); got != 0 {
		t.Fatalf("allocatedFileSize = %d, want 0 for a zero-block sparse file", got)
	}
}
