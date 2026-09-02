package torrent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cinemator/config"

	torrentlib "github.com/anacrolix/torrent"
)

func TestHLSDiskSizesAggregatesRenditionsByDownload(t *testing.T) {
	root := t.TempDir()
	firstID := strings.Repeat("a", 40)
	secondID := strings.Repeat("b", 40)
	want := map[string]int64{firstID: 0, secondID: 0}

	files := []struct {
		key  streamKey
		name string
		data string
	}{
		{key: streamKey{InfoHash: firstID, Index: 0, Audio: -1, Subtitle: -1}, name: "video.ts", data: "video"},
		{key: streamKey{InfoHash: firstID, Index: 1, Audio: -1, Subtitle: -1}, name: "audio.ts", data: "audio"},
		{key: streamKey{InfoHash: secondID, Index: 0, Audio: -1, Subtitle: -1}, name: "video.ts", data: "second"},
	}
	for _, file := range files {
		dir := filepath.Join(root, file.key.dirName())
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, file.name)
		if err := os.WriteFile(path, []byte(file.data), 0644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want[file.key.InfoHash] += allocatedFileSize(info)
	}

	invalidDir := filepath.Join(root, "not-an-info-hash_0_a-1_s-1")
	if err := os.MkdirAll(invalidDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidDir, "ignored.ts"), []byte("ignored"), 0644); err != nil {
		t.Fatal(err)
	}

	if got := hlsDiskSizes(root); !reflect.DeepEqual(got, want) {
		t.Fatalf("hlsDiskSizes() = %v, want %v", got, want)
	}
	if got := hlsDiskSize(root, firstID); got != want[firstID] {
		t.Fatalf("hlsDiskSize() = %d, want %d", got, want[firstID])
	}
}

func TestDownloadHasReadyHLSRequiresCompletedOutput(t *testing.T) {
	root := t.TempDir()
	id := strings.Repeat("a", 40)
	paths := (streamKey{InfoHash: id, Index: 0, Audio: -1, Subtitle: -1}).paths(root)
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.masterPlaylist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if downloadHasReadyHLS(root, id) {
		t.Fatal("incomplete HLS was treated as a retained artifact")
	}
	if err := markStreamOutputReady(paths); err != nil {
		t.Fatal(err)
	}
	if !downloadHasReadyHLS(root, id) {
		t.Fatal("completed HLS was not detected")
	}
}

func TestRemoveIncompleteDownloadHLSSkipsReservedOutput(t *testing.T) {
	root := t.TempDir()
	id := strings.Repeat("b", 40)
	key := streamKey{InfoHash: id, Index: 0, Audio: -1, Subtitle: -1}
	paths := key.paths(root)
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.videoPlaylist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}

	operationDone := make(chan struct{})
	m := &Manager{
		active:       make(map[streamKey]*streamInfo),
		preparations: make(map[streamKey]*preparationJob),
		streamOps:    map[streamKey]chan struct{}{key: operationDone},
		deletions:    make(map[string]chan struct{}),
		cfg:          config.Config{HLSPath: root},
	}
	if err := m.removeIncompleteDownloadHlsDirs(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.outDir); err != nil {
		t.Fatalf("reserved output was removed: %v", err)
	}

	m.finishStreamOperation(key, operationDone)
	if err := m.removeIncompleteDownloadHlsDirs(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.outDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreserved incomplete output was not removed: %v", err)
	}
}

func TestDefaultPreparationFileRequiresOneVideo(t *testing.T) {
	tests := []struct {
		name  string
		files []FileInfo
		want  int
		ok    bool
	}{
		{
			name: "video with sidecars",
			files: []FileInfo{
				{Index: 0, Name: "movie.mkv"},
				{Index: 1, Name: "movie.srt"},
				{Index: 2, Name: "poster.jpg"},
			},
			want: 0,
			ok:   true,
		},
		{
			name: "multiple videos",
			files: []FileInfo{
				{Index: 3, Name: "episode-1.mkv"},
				{Index: 4, Name: "episode-2.mkv"},
			},
			ok: false,
		},
		{
			name:  "single extensionless file",
			files: []FileInfo{{Index: 7, Name: "feature"}},
			want:  7,
			ok:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := defaultPreparationFile(tt.files)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("defaultPreparationFile() = %d, %v; want %d, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestResumePreparationsSkipsExpiredDownload(t *testing.T) {
	root := t.TempDir()
	hlsRoot := filepath.Join(root, "hls")
	downloadRoot := filepath.Join(root, "downloads")
	store, err := newDownloadStore(downloadRoot)
	if err != nil {
		t.Fatalf("newDownloadStore() error = %v", err)
	}

	id := strings.Repeat("c", 40)
	magnet := "magnet:?xt=urn:btih:" + id
	download, err := store.upsert(context.Background(), id, magnet, []FileInfo{{Index: 0, Name: "feature.mkv"}})
	if err != nil {
		t.Fatalf("upsert() error = %v", err)
	}
	expiresAt := time.Now().UTC().Add(-time.Hour)
	download.ExpiresAt = expiresAt
	if err := store.writeLockedForTest(download); err != nil {
		t.Fatalf("write expired download: %v", err)
	}

	m := &Manager{
		active:       make(map[streamKey]*streamInfo),
		preparations: make(map[streamKey]*preparationJob),
		deletions:    map[string]chan struct{}{id: make(chan struct{})},
		downloads:    store,
		cfg:          config.Config{HLSPath: hlsRoot, DownloadPath: downloadRoot},
	}
	m.resumePreparations()

	downloads, err := store.list(context.Background())
	if err != nil {
		t.Fatalf("list() error = %v", err)
	}
	if len(downloads) != 1 {
		t.Fatalf("list() returned %d downloads, want 1", len(downloads))
	}
	got := downloads[0]
	if got.Status != DownloadStatusExpired {
		t.Fatalf("status = %q, want %q", got.Status, DownloadStatusExpired)
	}
	if !got.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("expiresAt = %v, want %v", got.ExpiresAt, expiresAt)
	}
	if got.SelectedFileIndex != nil {
		t.Fatalf("selectedFileIndex = %d, want nil", *got.SelectedFileIndex)
	}
}

func TestStartHLSPreparationRestartsMissingRuntimeJob(t *testing.T) {
	root := t.TempDir()
	hlsRoot := filepath.Join(root, "hls")
	downloadRoot := filepath.Join(root, "downloads")
	store, err := newDownloadStore(downloadRoot)
	if err != nil {
		t.Fatalf("newDownloadStore() error = %v", err)
	}

	id := strings.Repeat("d", 40)
	magnet := "magnet:?xt=urn:btih:" + id
	if _, err := store.upsert(context.Background(), id, magnet, []FileInfo{{Index: 0, Name: "feature.mkv"}}); err != nil {
		t.Fatalf("upsert() error = %v", err)
	}
	if _, shouldStart, err := store.beginPreparation(context.Background(), id, 0); err != nil || !shouldStart {
		t.Fatalf("beginPreparation() = %v, %v", shouldStart, err)
	}

	operationDone := make(chan struct{})
	key := streamKey{InfoHash: id, Index: 0, Audio: -1, Subtitle: -1}
	m := &Manager{
		active:       make(map[streamKey]*streamInfo),
		preparations: make(map[streamKey]*preparationJob),
		streamOps:    make(map[streamKey]chan struct{}),
		torrents:     make(map[string]int),
		torrentOps:   map[string]chan struct{}{id: operationDone},
		deletions:    make(map[string]chan struct{}),
		downloads:    store,
		cfg:          config.Config{HLSPath: hlsRoot, DownloadPath: downloadRoot},
	}
	t.Cleanup(func() {
		m.mu.Lock()
		job := m.preparations[key]
		if job != nil {
			job.cancel()
		}
		m.mu.Unlock()
		if job != nil {
			select {
			case <-job.done:
			case <-time.After(time.Second):
				t.Error("preparation job did not stop")
			}
		}
		m.finishTorrentOperation(id, operationDone)
	})

	if err := m.StartHLSPreparation(context.Background(), magnet, 0); err != nil {
		t.Fatalf("StartHLSPreparation() error = %v", err)
	}
	m.mu.Lock()
	job := m.preparations[key]
	m.mu.Unlock()
	if job == nil {
		t.Fatal("StartHLSPreparation() did not restore the missing runtime job")
	}
	if err := m.StartHLSPreparation(context.Background(), magnet, 0); err != nil {
		t.Fatalf("second StartHLSPreparation() error = %v", err)
	}
	m.mu.Lock()
	retriedJob := m.preparations[key]
	m.mu.Unlock()
	if retriedJob != job {
		t.Fatal("second StartHLSPreparation() replaced the running job")
	}
}

func TestStartHLSPreparationSelectsOlderCachedRendition(t *testing.T) {
	root := t.TempDir()
	hlsRoot := filepath.Join(root, "hls")
	downloadRoot := filepath.Join(root, "downloads")
	store, err := newDownloadStore(downloadRoot)
	if err != nil {
		t.Fatalf("newDownloadStore() error = %v", err)
	}

	id := strings.Repeat("e", 40)
	magnet := "magnet:?xt=urn:btih:" + id
	files := []FileInfo{
		{Index: 0, Name: "episode-1.mkv"},
		{Index: 1, Name: "episode-2.mkv"},
	}
	if _, err := store.upsert(context.Background(), id, magnet, files); err != nil {
		t.Fatalf("upsert() error = %v", err)
	}
	if _, _, err := store.beginPreparation(context.Background(), id, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.finishPreparation(context.Background(), id, 0, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.beginPreparation(context.Background(), id, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.failPreparation(context.Background(), id, 1, errors.New("conversion failed")); err != nil {
		t.Fatal(err)
	}

	paths := (streamKey{InfoHash: id, Index: 0, Audio: -1, Subtitle: -1}).paths(hlsRoot)
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.masterPlaylist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := markStreamOutputReady(paths); err != nil {
		t.Fatal(err)
	}

	client, err := torrentlib.NewClient(torrentlib.TestingConfig(t))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { client.Close() })
	m := &Manager{
		client:     client,
		torrents:   make(map[string]int),
		torrentOps: make(map[string]chan struct{}),
		deletions:  make(map[string]chan struct{}),
		downloads:  store,
		cfg:        config.Config{HLSPath: hlsRoot, DownloadPath: downloadRoot},
	}
	if err := m.StartHLSPreparation(context.Background(), magnet, 0); err != nil {
		t.Fatalf("StartHLSPreparation() error = %v", err)
	}

	downloads, err := store.list(context.Background())
	if err != nil {
		t.Fatalf("list() error = %v", err)
	}
	got := downloads[0]
	if got.Status != DownloadStatusReady {
		t.Fatalf("status = %q, want %q", got.Status, DownloadStatusReady)
	}
	if got.SelectedFileIndex == nil || *got.SelectedFileIndex != 0 {
		t.Fatalf("selectedFileIndex = %v, want 0", got.SelectedFileIndex)
	}
	if got.ExpiresAt.Before(time.Now().Add(downloadDefaultTTL - time.Minute)) {
		t.Fatalf("expiresAt = %v, want a renewed lifetime", got.ExpiresAt)
	}
	selectedExpiry := got.ExpiresAt
	if err := store.finishPreparation(context.Background(), id, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	downloads, err = store.list(context.Background())
	if err != nil {
		t.Fatalf("list() after stale completion error = %v", err)
	}
	got = downloads[0]
	if got.SelectedFileIndex == nil || *got.SelectedFileIndex != 0 || !got.ExpiresAt.Equal(selectedExpiry) {
		t.Fatalf("stale completion replaced cached selection: %#v", got)
	}
}

func TestResumePreparationsCleansFailedPayload(t *testing.T) {
	root := t.TempDir()
	hlsRoot := filepath.Join(root, "hls")
	downloadRoot := filepath.Join(root, "downloads")
	store, err := newDownloadStore(downloadRoot)
	if err != nil {
		t.Fatalf("newDownloadStore() error = %v", err)
	}

	id := strings.Repeat("f", 40)
	magnet := "magnet:?xt=urn:btih:" + id
	files := []FileInfo{
		{Index: 0, Name: "episode-1.mkv"},
		{Index: 1, Name: "episode-2.mkv"},
	}
	if _, err := store.upsert(context.Background(), id, magnet, files); err != nil {
		t.Fatalf("upsert() error = %v", err)
	}
	if _, _, err := store.beginPreparation(context.Background(), id, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.finishPreparation(context.Background(), id, 0, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	paths := (streamKey{InfoHash: id, Index: 0, Audio: -1, Subtitle: -1}).paths(hlsRoot)
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.masterPlaylist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := markStreamOutputReady(paths); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.beginPreparation(context.Background(), id, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.failPreparation(context.Background(), id, 1, errors.New("conversion failed")); err != nil {
		t.Fatal(err)
	}
	incompletePaths := (streamKey{InfoHash: id, Index: 1, Audio: -1, Subtitle: -1}).paths(hlsRoot)
	if err := resetStreamOutput(incompletePaths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(incompletePaths.videoPlaylist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}
	payloadPath := filepath.Join(downloadRoot, id, "payload.bin")
	if err := os.WriteFile(payloadPath, []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}

	client, err := torrentlib.NewClient(torrentlib.TestingConfig(t))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { client.Close() })
	m := &Manager{
		client:     client,
		torrents:   make(map[string]int),
		torrentOps: make(map[string]chan struct{}),
		deletions:  make(map[string]chan struct{}),
		downloads:  store,
		cfg:        config.Config{HLSPath: hlsRoot, DownloadPath: downloadRoot},
	}
	m.resumePreparations()

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		_, err := os.Stat(payloadPath)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("failed payload was not cleaned during startup reconciliation")
		}
		time.Sleep(time.Millisecond)
	}

	downloads, err := store.list(context.Background())
	if err != nil {
		t.Fatalf("list() error = %v", err)
	}
	if len(downloads) != 1 || downloads[0].Status != DownloadStatusFailed {
		t.Fatalf("downloads = %#v, want one failed record", downloads)
	}
	if downloads[0].ExpiresAt.Before(time.Now().Add(downloadDefaultTTL - time.Minute)) {
		t.Fatalf("expiresAt = %v, want cached HLS retention", downloads[0].ExpiresAt)
	}
	if _, err := os.Stat(incompletePaths.outDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete HLS was not removed: %v", err)
	}
	ready, err := streamOutputReady(paths)
	if err != nil || !ready {
		t.Fatalf("completed cached HLS was removed: ready=%v, err=%v", ready, err)
	}
}
