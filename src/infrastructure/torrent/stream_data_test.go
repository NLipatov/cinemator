package torrent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStreamKeyDirNameAndParse(t *testing.T) {
	key := streamKey{InfoHash: "abc123", Index: 7, Audio: 1, Subtitle: -1}
	if got := key.dirName(); got != "abc123_7_a1_s-1" {
		t.Fatalf("dirName() = %q", got)
	}
	parsed, err := parseStreamDir(key.dirName())
	if err != nil || parsed != key {
		t.Fatalf("parseStreamDir() = %#v, %v; want %#v", parsed, err, key)
	}
}

func TestStreamKeyPaths(t *testing.T) {
	root := t.TempDir()
	paths := (streamKey{InfoHash: "hash", Index: 2, Audio: 0, Subtitle: 3}).paths(root)
	wantDir := filepath.Join(root, "hash_2_a0_s3")
	if paths.outDir != wantDir ||
		paths.videoPlaylist != filepath.Join(wantDir, "index.m3u8") ||
		paths.subtitlePlaylist != filepath.Join(wantDir, "subs.m3u8") ||
		paths.masterPlaylist != filepath.Join(wantDir, "master.m3u8") {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestParseStreamDirRejectsMalformedNames(t *testing.T) {
	for _, name := range []string{"", "hash_1_a0", "hash_x_a0_s0", "hash_1_x0_s0", "hash_1_a0_x0", "hash_1_a0_sx"} {
		if _, err := parseStreamDir(name); err == nil {
			t.Fatalf("parseStreamDir(%q) succeeded", name)
		}
	}
}

func TestResetStreamOutputRemovesStaleHLSFiles(t *testing.T) {
	paths := streamKey{InfoHash: "hash", Index: 1, Audio: 0, Subtitle: -1}.paths(t.TempDir())
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(paths.outDir, "chunk_000001.ts")
	if err := os.WriteFile(stale, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := resetStreamOutput(paths); err != nil {
		t.Fatalf("resetStreamOutput() error = %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale file still exists: %v", err)
	}
}
