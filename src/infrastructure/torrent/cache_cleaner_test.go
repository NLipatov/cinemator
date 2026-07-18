package torrent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
	"cinemator/presentation/settings"
)

func TestEnforceCacheLimitEvictsActiveGeneratedAssetsButProtectsManifestsAndWorkFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	t.Setenv("CINEMATOR_MAX_CACHE_BYTES", "400")
	key := streamKey{InfoHash: "hash", Index: 0, Audio: 0, Subtitle: -1}
	paths := key.paths(root)
	workDir := filepath.Join(paths.outDir, ".generating-test")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}

	files := []string{
		paths.masterPlaylist,
		paths.videoPlaylist,
		filepath.Join(paths.outDir, "chunk_000000.ts"),
		filepath.Join(paths.outDir, "chunk_000001.ts"),
		filepath.Join(workDir, "chunk_000002.ts"),
		filepath.Join(paths.outDir, ".remuxing-test", "part_000000.ts"),
	}
	for index, path := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 100), 0644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(index+1), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	m := &manager{
		active:   map[streamKey]*streamInfo{key: readyCacheTestStream(paths)},
		settings: settings.NewSettings(),
	}
	m.assets, _ = newHlsAssetStore(root)
	m.enforceCacheLimit()

	if _, err := os.Stat(filepath.Join(paths.outDir, "chunk_000000.ts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old generated segment was not evicted: %v", err)
	}
	for _, path := range []string{paths.masterPlaylist, paths.videoPlaylist, filepath.Join(workDir, "chunk_000002.ts"), filepath.Join(paths.outDir, ".remuxing-test", "part_000000.ts")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected cache file %s was removed: %v", path, err)
		}
	}
}

func TestReserveHlsGenerationCreatesAndReleasesHardHeadroom(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	t.Setenv("CINEMATOR_MAX_CACHE_BYTES", "10485760")
	old := filepath.Join(root, "old", "chunk_000000.ts")
	if err := os.MkdirAll(filepath.Dir(old), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(old, 6<<20); err != nil {
		t.Fatal(err)
	}
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	m := &manager{active: make(map[streamKey]*streamInfo), assets: assets, settings: settings.NewSettings()}

	release, err := m.reserveHlsGeneration(6*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old segment was not evicted to create headroom: %v", err)
	}
	if m.hlsReserved == 0 {
		t.Fatal("reservation was not recorded")
	}
	release()
	if m.hlsReserved != 0 {
		t.Fatalf("reserved bytes after release = %d", m.hlsReserved)
	}
}

func TestEstimatedHlsWindowBytesUsesSourceBitrate(t *testing.T) {
	low := estimatedHlsWindowBytes(30*time.Second, 0)
	high := estimatedHlsWindowBytes(30*time.Second, 20_000_000)
	if high <= low {
		t.Fatalf("high-bitrate reservation = %d, want more than %d", high, low)
	}
}

func TestCacheDoesNotUnlinkLeasedActiveSegment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	t.Setenv("CINEMATOR_MAX_CACHE_BYTES", "1")
	key := streamKey{InfoHash: "hash", Index: 0, Audio: 0, Subtitle: -1}
	paths := key.paths(root)
	segment := filepath.Join(paths.outDir, "chunk_000000.ts")
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(segment, []byte("recent"), 0644); err != nil {
		t.Fatal(err)
	}
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := assets.Open(segment)
	if err != nil {
		t.Fatal(err)
	}
	m := &manager{active: map[streamKey]*streamInfo{key: readyCacheTestStream(paths)}, assets: assets, settings: settings.NewSettings()}
	m.enforceCacheLimit()
	if _, err := os.Stat(segment); err != nil {
		t.Fatalf("leased active segment was evicted: %v", err)
	}
	if err := asset.Close(); err != nil {
		t.Fatal(err)
	}
	m.enforceCacheLimit()
	if _, err := os.Stat(segment); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released segment was not evicted: %v", err)
	}
}

func readyCacheTestStream(paths streamPaths) *streamInfo {
	ready := make(chan struct{})
	close(ready)
	return &streamInfo{
		paths:         paths,
		ready:         ready,
		assetVersion:  "old",
		mediaInfo:     domain.MediaInfo{Duration: 12},
		selection:     ffmpeg.StreamSelection{SubtitleTrackIndex: -1},
		directWindows: make(map[int][]ffmpeg.HLSFragment),
	}
}

func TestActiveAssetEvictionRotatesImmutableGeneration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	t.Setenv("CINEMATOR_MAX_CACHE_BYTES", "1")
	key := streamKey{InfoHash: "hash", Index: 0, Audio: 0, Subtitle: -1}
	paths := key.paths(root)
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	segments := []string{
		filepath.Join(paths.outDir, "chunk_000000.ts"),
		filepath.Join(paths.outDir, "chunk_000001.ts"),
	}
	for _, segment := range segments {
		if err := os.WriteFile(segment, []byte("segment"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	ready := make(chan struct{})
	close(ready)
	stream := &streamInfo{
		paths:         paths,
		ready:         ready,
		assetVersion:  "old",
		mediaInfo:     domain.MediaInfo{Duration: 12},
		selection:     ffmpeg.StreamSelection{SubtitleTrackIndex: -1},
		directWindows: make(map[int][]ffmpeg.HLSFragment),
	}
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	m := &manager{active: map[streamKey]*streamInfo{key: stream}, assets: assets, settings: settings.NewSettings()}
	m.enforceCacheLimit()
	for _, segment := range segments {
		if _, err := os.Stat(segment); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("segment was not evicted: %v", err)
		}
	}
	stream.mtx.Lock()
	version := stream.assetVersion
	stream.mtx.Unlock()
	if version == "old" || version == "" {
		t.Fatalf("asset version was not rotated: %q", version)
	}
	playlist, err := os.ReadFile(paths.videoPlaylist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(playlist), "?v="+version) {
		t.Fatalf("rotated playlist does not use version %q:\n%s", version, playlist)
	}
}
