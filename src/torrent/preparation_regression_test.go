package torrent

import (
	"context"
	"crypto/sha1"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cinemator/config"

	torrentlib "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
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

func TestStartStreamWaitsForTorrentCleanup(t *testing.T) {
	for _, cancelStart := range []bool{false, true} {
		name := "resume"
		if cancelStart {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			store, err := newDownloadStore(filepath.Join(root, "downloads"))
			if err != nil {
				t.Fatal(err)
			}
			closing, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			cfg := torrentlib.TestingConfig(t)
			cfg.DefaultStorage = &delayedCloseStorage{
				ClientImplCloser: storage.NewFileByInfoHash(store.root),
				beforeClose: sync.OnceFunc(func() {
					close(closing)
					<-release
				}),
			}
			client, err := torrentlib.NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { client.Close() })
			t.Cleanup(unblock)
			data := []byte{0}
			pieceHash := sha1.Sum(data)
			info, err := bencode.Marshal(metainfo.Info{Name: "movie.mkv", Length: 1, PieceLength: 1, Pieces: pieceHash[:]})
			if err != nil {
				t.Fatal(err)
			}
			meta := metainfo.MetaInfo{InfoBytes: info}
			id := meta.HashInfoBytes().HexString()
			magnet := "magnet:?xt=urn:btih:" + id
			if _, err := store.upsert(t.Context(), id, magnet, []FileInfo{{Index: 0, Name: "movie.mkv", Size: 1}}); err != nil {
				t.Fatal(err)
			}
			if err := store.finishPreparation(t.Context(), id, 0, time.Now()); err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(store.downloadDir(id), "movie.mkv")
			if err := os.WriteFile(payload, data, 0644); err != nil {
				t.Fatal(err)
			}
			// Create valid data before background verification starts, so the file
			// cannot be renamed to .part while cleanup is waiting to close storage.
			source, err := client.AddTorrent(&meta)
			if err != nil {
				t.Fatal(err)
			}
			key := streamKey{InfoHash: id, Index: 0, Audio: -1, Subtitle: -1}
			m := &Manager{client: client, downloads: store, active: make(map[streamKey]*streamInfo), cfg: config.Config{HLSPath: filepath.Join(root, "hls")}}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cleanupDone := m.cleanupTransientPayload(id, source)
			t.Cleanup(func() {
				unblock()
				<-cleanupDone
				if err := m.cleanup(context.Background(), key); err != nil {
					t.Error(err)
				}
			})
			select {
			case <-closing:
			case <-ctx.Done():
				t.Fatal("cleanup did not start closing storage")
			}
			// Cleanup has already decided the source is unused, but has not yet
			// removed its payload. Registration must wait for the whole operation.
			observed := &doneObservedContext{Context: ctx, requested: make(chan struct{})}
			result := make(chan error, 1)
			go func() {
				_, err := m.startStream(observed, magnet, key)
				result <- err
			}()
			select {
			case <-observed.requested:
			case err := <-result:
				t.Fatalf("startStream returned before source cleanup finished: %v", err)
			case <-ctx.Done():
				t.Fatal("startStream did not wait for source cleanup")
			}
			m.mu.Lock()
			registered := m.active[key] != nil
			m.mu.Unlock()
			if registered {
				t.Fatal("stream registered after cleanup decided to remove its source")
			}
			if _, err := os.Stat(payload); err != nil {
				t.Fatal("cleanup removed payload before storage closed:", err)
			}
			if cancelStart {
				cancel()
			} else {
				unblock()
			}
			select {
			case err := <-result:
				if cancelStart && !errors.Is(err, context.Canceled) || !cancelStart && err != nil {
					t.Fatalf("startStream returned %v (cancel=%v)", err, cancelStart)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("startStream did not resume or honor cancellation")
			}
			unblock()
			<-cleanupDone
			if _, err := os.Stat(payload); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cleanup retained the old source payload: %v", err)
			}
		})
	}
}

type delayedCloseStorage struct {
	storage.ClientImplCloser
	beforeClose func()
}

func (s *delayedCloseStorage) OpenTorrent(ctx context.Context, info *metainfo.Info, hash metainfo.Hash) (storage.TorrentImpl, error) {
	t, err := s.ClientImplCloser.OpenTorrent(ctx, info, hash)
	if err == nil {
		closeStorage := t.Close
		t.Close = func() error {
			s.beforeClose()
			return closeStorage()
		}
	}
	return t, err
}

func TestLaunchPreparationRecordsCachedInspectionFailure(t *testing.T) {
	root := t.TempDir()
	store, err := newDownloadStore(filepath.Join(root, "downloads"))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 40)
	magnet := "magnet:?xt=urn:btih:" + id
	if _, err := store.upsert(t.Context(), id, magnet, []FileInfo{{Index: 0, Name: "movie.mkv"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.beginPreparation(t.Context(), id, 0); err != nil {
		t.Fatal(err)
	}
	key := streamKey{InfoHash: id, Index: 0, Audio: -1, Subtitle: -1}
	hlsRoot := filepath.Join(root, "hls")
	paths := key.paths(hlsRoot)
	// Simulate an unreadable marker on a previously completed, active rendition.
	if err := os.MkdirAll(paths.readyMarker, 0755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{downloads: store, cfg: config.Config{HLSPath: hlsRoot}, active: map[streamKey]*streamInfo{key: {completed: true, paths: paths}}}
	m.launchPreparation(magnet, id, 0)
	downloads, err := m.ListDownloads(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(downloads) != 1 || downloads[0].Status != DownloadStatusFailed || downloads[0].PreparationErr == "" {
		t.Fatalf("cached inspection failure did not reach a terminal state: %+v", downloads)
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
