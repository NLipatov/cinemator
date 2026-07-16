package torrent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	}
	for index, path := range files {
		if err := os.WriteFile(path, make([]byte, 100), 0644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(index+1), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	m := &manager{
		active:   map[streamKey]*streamInfo{key: {paths: paths}},
		settings: settings.NewSettings(),
	}
	m.enforceCacheLimit()

	if _, err := os.Stat(filepath.Join(paths.outDir, "chunk_000000.ts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old generated segment was not evicted: %v", err)
	}
	for _, path := range []string{paths.masterPlaylist, paths.videoPlaylist, filepath.Join(workDir, "chunk_000002.ts")} {
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
	m := &manager{active: make(map[streamKey]*streamInfo), settings: settings.NewSettings()}

	release, err := m.reserveHlsGeneration(1)
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
