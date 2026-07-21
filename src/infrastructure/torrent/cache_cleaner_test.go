package torrent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
	"cinemator/presentation/settings"
)

func TestSharedCachePreservesHlsBeforeTorrentPieces(t *testing.T) {
	root := t.TempDir()
	hlsRoot := filepath.Join(root, "hls")
	downloadRoot := filepath.Join(root, "download")
	t.Setenv("CINEMATOR_HLS_PATH", hlsRoot)
	if err := os.MkdirAll(hlsRoot, 0755); err != nil {
		t.Fatal(err)
	}
	hls := filepath.Join(hlsRoot, "history.m4s")
	if err := os.WriteFile(hls, []byte("cached HLS"), 0644); err != nil {
		t.Fatal(err)
	}
	hlsInfo, err := os.Stat(hls)
	if err != nil {
		t.Fatal(err)
	}
	budget := newCacheBudget(1 << 20)
	pieceCache, err := newPieceCache(downloadRoot, budget, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"completed/old", "completed/new"} {
		instance, _ := pieceCache.provider.NewInstance(name)
		if err := instance.(*pieceCacheInstance).PutSized(bytes.NewReader([]byte("data")), 4); err != nil {
			t.Fatal(err)
		}
	}
	budget.limit = allocatedFileSize(hlsInfo) + 4
	assets, err := newHlsAssetStore(hlsRoot)
	if err != nil {
		t.Fatal(err)
	}
	m := &manager{
		active:   make(map[streamKey]*streamInfo),
		media:    &mediaCache{assets: assets, pieces: pieceCache.provider, budget: budget},
		settings: settings.NewSettings(),
	}

	m.enforceCacheLimit()

	if _, err := os.Stat(hls); err != nil {
		t.Fatalf("HLS history was evicted before reproducible pieces: %v", err)
	}
	if got := pieceCache.provider.usedBytes(); got > 4 {
		t.Fatalf("piece cache uses %d bytes, want at most shared remainder", got)
	}
}

func TestEnforceCacheLimitPreservesEveryActivePresentationFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", "400")
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
	assets, _ := newHlsAssetStore(root)
	m.media = &mediaCache{budget: newCacheBudget(m.settings.MaxCacheBytes()), assets: assets}
	err := m.media.enforce(root, m.hlsCacheProtection)
	if err == nil {
		t.Fatal("cache enforcement unexpectedly fit active presentation under target")
	}

	for _, path := range files {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("active presentation file %s was removed: %v", path, err)
		}
	}
}

func TestReserveHlsGenerationCreatesAndReleasesHardHeadroom(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", "10485760")
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
	m := &manager{active: make(map[streamKey]*streamInfo), media: &mediaCache{assets: assets}, settings: settings.NewSettings()}
	m.media.budget = newCacheBudget(m.settings.MaxCacheBytes())

	release, err := m.reserveHlsGeneration(6*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old segment was not evicted to create headroom: %v", err)
	}
	if m.media.budget.hlsReserved == 0 {
		t.Fatal("reservation was not recorded")
	}
	release()
	if m.media.budget.hlsReserved != 0 {
		t.Fatalf("reserved bytes after release = %d", m.media.budget.hlsReserved)
	}
}

func TestEstimatedHlsWindowBytesUsesSourceBitrate(t *testing.T) {
	low := estimatedHlsWindowBytes(30*time.Second, 0)
	high := estimatedHlsWindowBytes(30*time.Second, 20_000_000)
	if high <= low {
		t.Fatalf("high-bitrate reservation = %d, want more than %d", high, low)
	}
}

func TestHlsCacheProtectionIncludesActivePresentationDirectory(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 0, Audio: 0, Subtitle: -1}
	paths := key.paths(t.TempDir())
	stream := readyCacheTestStream(paths)
	manager := &manager{active: map[streamKey]*streamInfo{key: stream}, settings: settings.NewSettings()}

	active := manager.hlsCacheProtection()
	if _, ok := active[key.dirName()]; !ok {
		t.Fatalf("active presentation directory %q is not protected", paths.outDir)
	}
}

func TestHlsCachePressurePreservesActivePresentation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	key := streamKey{InfoHash: "hash", Index: 0, Audio: 0, Subtitle: -1}
	paths := key.paths(root)
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	stream := readyCacheTestStream(paths)
	stream.directPlay = true
	stream.playbackWindow = 30
	stream.mediaInfo = domain.MediaInfo{Duration: 120, VideoCodec: "h264"}
	stream.videoJobs = map[*segmentJob]struct{}{
		{begin: 60, end: 75, done: make(chan struct{})}: {},
	}
	owners := []int{0, 15, 30, 45}
	var total int64
	var oldestSize int64
	for index, owner := range owners {
		name := fmt.Sprintf("direct_%06d_0000.m4s", owner)
		path := filepath.Join(paths.outDir, name)
		if err := os.WriteFile(path, []byte("materialized history"), 0644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(index+1), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		size := allocatedFileSize(info)
		total += size
		if index == 0 {
			oldestSize = size
		}
		stream.directWindows[owner] = []ffmpeg.HLSFragment{{
			Start:    float64(owner * 2),
			Duration: 30,
			Name:     name,
		}}
	}
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", strconv.FormatInt(total-oldestSize, 10))
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := &manager{
		active:   map[streamKey]*streamInfo{key: stream},
		media:    &mediaCache{assets: assets},
		settings: settings.NewSettings(),
	}
	manager.media.budget = newCacheBudget(manager.settings.MaxCacheBytes())

	err = manager.media.enforce(root, manager.hlsCacheProtection)
	if err == nil {
		t.Fatal("cache enforcement unexpectedly fit active presentation under target")
	}

	for _, owner := range owners {
		path := filepath.Join(paths.outDir, fmt.Sprintf("direct_%06d_0000.m4s", owner))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("active presentation asset %d was removed: %v", owner, err)
		}
	}
	stream.mtx.Lock()
	defer stream.mtx.Unlock()
	for _, owner := range owners {
		if _, ok := stream.directWindows[owner]; !ok {
			t.Fatalf("active presentation window %d was unpublished", owner)
		}
	}
}

func TestCacheNeverEvictsActiveSegments(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", "1")
	key := streamKey{InfoHash: "hash", Index: 0, Audio: 0, Subtitle: -1}
	paths := key.paths(root)
	segment := filepath.Join(paths.outDir, "chunk_000000.ts")
	reclaimable := filepath.Join(paths.outDir, "chunk_000001.ts")
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(segment, []byte("recent"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reclaimable, []byte("reclaimable"), 0644); err != nil {
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
	m := &manager{active: map[streamKey]*streamInfo{key: readyCacheTestStream(paths)}, media: &mediaCache{assets: assets}, settings: settings.NewSettings()}
	m.media.budget = newCacheBudget(m.settings.MaxCacheBytes())
	m.enforceCacheLimit()
	if _, err := os.Stat(segment); err != nil {
		t.Fatalf("leased active segment was evicted: %v", err)
	}
	if _, err := os.Stat(reclaimable); err != nil {
		t.Fatalf("another active segment was evicted: %v", err)
	}
	if err := asset.Close(); err != nil {
		t.Fatal(err)
	}
	m.enforceCacheLimit()
	if _, err := os.Stat(segment); err != nil {
		t.Fatalf("released active segment was evicted: %v", err)
	}
}

func TestCachePreservesGlobalLRUWhenOldestActiveAssetIsLeased(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	key := streamKey{InfoHash: "hash", Index: 0, Audio: 0, Subtitle: -1}
	paths := key.paths(root)
	oldest := filepath.Join(paths.outDir, "chunk_000000.ts")
	middle := filepath.Join(root, "inactive", "chunk_000000.ts")
	newest := filepath.Join(paths.outDir, "chunk_000001.ts")
	files := []string{oldest, middle, newest}
	for index, path := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("segment"), 0644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(index+1), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	var total int64
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		total += allocatedFileSize(info)
	}
	middleInfo, err := os.Stat(middle)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", strconv.FormatInt(total-allocatedFileSize(middleInfo), 10))

	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := assets.Open(oldest)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	m := &manager{active: map[streamKey]*streamInfo{key: readyCacheTestStream(paths)}, media: &mediaCache{assets: assets}, settings: settings.NewSettings()}
	m.media.budget = newCacheBudget(m.settings.MaxCacheBytes())
	m.enforceCacheLimit()

	if _, err := os.Stat(oldest); err != nil {
		t.Fatalf("leased oldest segment was removed: %v", err)
	}
	if _, err := os.Stat(middle); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("next-oldest inactive segment was not removed: %v", err)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("newer active segment was removed ahead of global LRU: %v", err)
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

func TestActiveAssetEvictionNeverRotatesPresentationGeneration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", "1")
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
	m := &manager{active: map[streamKey]*streamInfo{key: stream}, media: &mediaCache{assets: assets}, settings: settings.NewSettings()}
	m.media.budget = newCacheBudget(m.settings.MaxCacheBytes())
	err = m.media.enforce(root, m.hlsCacheProtection)
	if err == nil {
		t.Fatal("cache enforcement unexpectedly fit active presentation under target")
	}
	for _, segment := range segments {
		if _, err := os.Stat(segment); err != nil {
			t.Fatalf("active segment was removed: %v", err)
		}
	}
	stream.mtx.Lock()
	version := stream.assetVersion
	stream.mtx.Unlock()
	if version != "old" {
		t.Fatalf("asset version changed under active playback: %q", version)
	}
}

func TestCacheContinuesPastFailedActiveBatch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	key := streamKey{InfoHash: "hash", Index: 0, Audio: 0, Subtitle: 0}
	paths := key.paths(root)
	active := filepath.Join(paths.outDir, "chunk_000000.ts")
	inactive := filepath.Join(root, "inactive", "chunk_000000.ts")
	for index, path := range []string{active, inactive} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("segment"), 0644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(index+1), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	inactiveInfo, err := os.Stat(inactive)
	if err != nil {
		t.Fatal(err)
	}
	target := allocatedFileSize(inactiveInfo)
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", strconv.FormatInt(target, 10))
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	stream := readyCacheTestStream(paths)
	stream.selection.SubtitleTrackIndex = 0
	m := &manager{active: map[streamKey]*streamInfo{key: stream}, media: &mediaCache{assets: assets}, settings: settings.NewSettings()}
	m.media.budget = newCacheBudget(m.settings.MaxCacheBytes())
	m.enforceCacheLimit()

	if _, err := os.Stat(active); err != nil {
		t.Fatalf("failed active batch unexpectedly removed its segment: %v", err)
	}
	if _, err := os.Stat(inactive); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache did not continue to the later reclaimable candidate: %v", err)
	}
}
