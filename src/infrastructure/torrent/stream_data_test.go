package torrent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStreamKeyDirNameAndParse(t *testing.T) {
	key := streamKey{InfoHash: "abc123", Index: 7, Audio: 1, Subtitle: -1}

	gotName := key.dirName()
	if gotName != "abc123_7_a1_s-1" {
		t.Fatalf("dirName() = %q", gotName)
	}

	gotKey, err := parseStreamDir(gotName)
	if err != nil {
		t.Fatalf("parseStreamDir() error = %v", err)
	}
	if gotKey != key {
		t.Fatalf("parseStreamDir() = %#v, want %#v", gotKey, key)
	}
}

func TestStreamKeyPaths(t *testing.T) {
	root := t.TempDir()
	key := streamKey{InfoHash: "hash", Index: 2, Audio: 0, Subtitle: 3}

	got := key.paths(root)
	wantDir := filepath.Join(root, "hash_2_a0_s3")
	if got.outDir != wantDir {
		t.Fatalf("outDir = %q, want %q", got.outDir, wantDir)
	}
	if got.videoPlaylist != filepath.Join(wantDir, "index.m3u8") {
		t.Fatalf("videoPlaylist = %q", got.videoPlaylist)
	}
	if got.subtitlePlaylist != filepath.Join(wantDir, "subs.m3u8") {
		t.Fatalf("subtitlePlaylist = %q", got.subtitlePlaylist)
	}
	if got.masterPlaylist != filepath.Join(wantDir, "master.m3u8") {
		t.Fatalf("masterPlaylist = %q", got.masterPlaylist)
	}
}

func TestParseStreamDirRejectsMalformedNames(t *testing.T) {
	cases := []string{
		"",
		"hash_1_a0",
		"hash_x_a0_s0",
		"hash_1_x0_s0",
		"hash_1_a0_x0",
		"hash_1_a0_sx",
	}

	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseStreamDir(name); err == nil {
				t.Fatalf("parseStreamDir(%q) succeeded, want error", name)
			}
		})
	}
}

func TestStreamInfoWaitReadyReturnsSignalError(t *testing.T) {
	want := errors.New("probe failed")
	s := &streamInfo{}
	s.resetReady()
	s.signalReady(want)

	if got := s.waitReady(context.Background()); !errors.Is(got, want) {
		t.Fatalf("waitReady() = %v, want %v", got, want)
	}
}

func TestStreamInfoWaitReadyHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()

	s := &streamInfo{}
	s.resetReady()

	if err := s.waitReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitReady() = %v, want deadline exceeded", err)
	}
}
