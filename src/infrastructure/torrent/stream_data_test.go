package torrent

import (
	"cinemator/domain"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStreamKeyDirNameAndParse(t *testing.T) {
	key := streamKey{InfoHash: "abc123", Index: 7, Audio: 1, Subtitle: -1}
	if got := key.dirName(); got != "abc123_7_a1_s-1_t0" {
		t.Fatalf("dirName() = %q", got)
	}
	parsed, err := parseStreamDir(key.dirName())
	if err != nil || parsed != key {
		t.Fatalf("parseStreamDir() = %#v, %v; want %#v", parsed, err, key)
	}
}

func TestTranscodedStreamKeyHasDistinctStableIdentity(t *testing.T) {
	key := streamKey{InfoHash: "abc123", Index: 7, Audio: 1, Subtitle: -1, Transcode: true}
	if got := key.dirName(); got != "abc123_7_a1_s-1_t1" {
		t.Fatalf("dirName() = %q", got)
	}
	parsed, err := parseStreamDir(key.dirName())
	if err != nil || parsed != key {
		t.Fatalf("parseStreamDir() = %#v, %v; want %#v", parsed, err, key)
	}
	if got := key.paths("/cache").outDir; got != filepath.Join("/cache", key.dirName()) {
		t.Fatalf("transcoded output path = %q", got)
	}
}

func TestPresentationStartHasDistinctStableIdentity(t *testing.T) {
	key := streamKey{InfoHash: "abc123", Index: 7, Audio: 1, Subtitle: -1, Start: 12_345}
	if got := key.dirName(); got != "abc123_7_a1_s-1_t0_p12345" {
		t.Fatalf("dirName() = %q", got)
	}
	parsed, err := parseStreamDir(key.dirName())
	if err != nil || parsed != key {
		t.Fatalf("parseStreamDir() = %#v, %v; want %#v", parsed, err, key)
	}
}

func TestRecordSourceBytesUpdatesOnlyRequestedVideoPreparation(t *testing.T) {
	requested := &segmentJob{begin: 10, end: 15, id: "requested"}
	background := &segmentJob{begin: 0, end: 5, id: "background", background: true}
	subtitle := &segmentJob{begin: 10, end: 15, id: "subtitle"}
	started := time.Now().Add(-time.Minute)
	stream := &streamInfo{
		status:        domain.HlsStatus{Phase: "preparing", LastProgress: started},
		statusSegment: 10,
		videoJobs: map[*segmentJob]struct{}{
			requested:  {},
			background: {},
		},
		subtitleJobs: map[*segmentJob]struct{}{subtitle: {}},
	}

	stream.recordSourceBytes(background.id, 10)
	stream.recordSourceBytes(subtitle.id, 20)
	if stream.status.BytesRead != 0 || !stream.status.LastProgress.Equal(started) {
		t.Fatalf("unrelated work changed preparation status: %+v", stream.status)
	}
	stream.recordSourceBytes(requested.id, 30)
	if stream.status.BytesRead != 30 || !stream.status.LastProgress.After(started) {
		t.Fatalf("requested video work did not advance preparation status: %+v", stream.status)
	}
	if background.bytesRead != 10 || subtitle.bytesRead != 20 || requested.bytesRead != 30 {
		t.Fatalf("per-job byte accounting was lost: background=%d subtitle=%d requested=%d", background.bytesRead, subtitle.bytesRead, requested.bytesRead)
	}
}

func TestStreamKeyPaths(t *testing.T) {
	root := t.TempDir()
	paths := (streamKey{InfoHash: "hash", Index: 2, Audio: 0, Subtitle: 3}).paths(root)
	wantDir := filepath.Join(root, "hash_2_a0_s3_t0")
	if paths.outDir != wantDir ||
		paths.videoPlaylist != filepath.Join(wantDir, "index.m3u8") ||
		paths.subtitlePlaylist != filepath.Join(wantDir, "subs.m3u8") ||
		paths.masterPlaylist != filepath.Join(wantDir, "master.m3u8") {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestParseStreamDirRejectsMalformedNames(t *testing.T) {
	for _, name := range []string{"", "hash_1_a0", "hash_x_a0_s0_t0", "hash_1_x0_s0_t0", "hash_1_a0_x0_t0", "hash_1_a0_sx_t0", "hash_1_a0_s0_tx"} {
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
	assets, err := newHlsAssetStore(filepath.Dir(paths.outDir))
	if err != nil {
		t.Fatal(err)
	}
	m := manager{assets: assets}
	if err := m.resetStreamOutput(paths); err != nil {
		t.Fatalf("resetStreamOutput() error = %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale file still exists: %v", err)
	}
}
