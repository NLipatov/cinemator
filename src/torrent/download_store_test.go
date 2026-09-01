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

	"cinemator/media"
)

func TestDownloadStoreLifecycle(t *testing.T) {
	store, err := newDownloadStore(t.TempDir())
	if err != nil {
		t.Fatalf("newDownloadStore() error = %v", err)
	}

	id := strings.Repeat("a", 40)
	files := []FileInfo{
		{Index: 0, Name: "sample.txt", Size: 4},
		{Index: 1, Name: "Movies/Feature.mkv", Size: 10},
	}
	download, err := store.upsert(context.Background(), id, "magnet:?xt=urn:btih:"+id, files)
	if err != nil {
		t.Fatalf("upsert() error = %v", err)
	}
	if download.Title != "Feature.mkv" {
		t.Fatalf("download title = %q, want Feature.mkv", download.Title)
	}
	if download.Size != 14 {
		t.Fatalf("download size = %d, want 14", download.Size)
	}
	if download.DiskSize != 0 {
		t.Fatalf("download disk size = %d, want 0", download.DiskSize)
	}
	if download.Status != DownloadStatusReady {
		t.Fatalf("download status = %q, want ready", download.Status)
	}
	if _, err := os.Stat(filepath.Join(store.root, id, downloadStoreDirName, "metadata.json")); err != nil {
		t.Fatalf("metadata not written inside download dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.root, id, "payload.bin"), []byte("payload"), 0644); err != nil {
		t.Fatalf("write payload file: %v", err)
	}

	downloads, err := store.list(context.Background())
	if err != nil {
		t.Fatalf("list() error = %v", err)
	}
	if len(downloads) != 1 || downloads[0].ID != id {
		t.Fatalf("list() = %#v, want one download with id %s", downloads, id)
	}
	if downloads[0].DiskSize <= 0 {
		t.Fatalf("listed download disk size = %d, want positive", downloads[0].DiskSize)
	}

	extended, err := store.extend(context.Background(), id, 24*time.Hour)
	if err != nil {
		t.Fatalf("extend() error = %v", err)
	}
	if !extended.ExpiresAt.After(download.ExpiresAt) {
		t.Fatalf("extended expiry = %v, want after %v", extended.ExpiresAt, download.ExpiresAt)
	}

	if err := store.delete(context.Background(), id); err != nil {
		t.Fatalf("delete() error = %v", err)
	}
	downloads, err = store.list(context.Background())
	if err != nil {
		t.Fatalf("list() after delete error = %v", err)
	}
	if len(downloads) != 0 {
		t.Fatalf("list() after delete = %#v, want empty", downloads)
	}
}

func TestCleanInfoHashRejectsBadIDs(t *testing.T) {
	for _, id := range []string{"", "abc", strings.Repeat("x", 40), strings.Repeat("a", 39)} {
		if _, err := cleanInfoHash(id); !errors.Is(err, ErrBadDownloadID) {
			t.Fatalf("cleanInfoHash(%q) error = %v, want ErrBadDownloadID", id, err)
		}
	}
}

func TestDeletePayloadPreservesDownloadMetadata(t *testing.T) {
	store, err := newDownloadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("b", 40)
	if _, err := store.upsert(context.Background(), id, "magnet:?xt=urn:btih:"+id, nil); err != nil {
		t.Fatal(err)
	}
	mediaInfo := media.MediaInfo{
		VideoCodec:  "hevc",
		NeedFilter:  true,
		AudioTracks: []media.AudioTrack{{Index: 0, Codec: "aac", Language: "eng"}},
		Subtitles:   []media.SubtitleTrack{{Index: 0, Codec: "subrip", Language: "eng"}},
	}
	if err := store.saveMediaInfo(context.Background(), id, 0, mediaInfo); err != nil {
		t.Fatalf("saveMediaInfo() error = %v", err)
	}
	payloadDir := filepath.Join(store.downloadDir(id), "movie")
	if err := os.MkdirAll(payloadDir, 0755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(payloadDir, "feature.mkv")
	if err := os.WriteFile(payload, []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := store.deletePayload(context.Background(), id); err != nil {
		t.Fatalf("deletePayload() error = %v", err)
	}
	if _, err := os.Stat(payload); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("payload still exists: %v", err)
	}
	if _, err := os.Stat(store.metadataPath(id)); err != nil {
		t.Fatalf("metadata was removed: %v", err)
	}
	cached, found, err := store.loadMediaInfo(context.Background(), id, 0)
	if err != nil || !found || !reflect.DeepEqual(cached, mediaInfo) {
		t.Fatalf("loadMediaInfo() = %#v, %v, %v; want preserved media info", cached, found, err)
	}
	downloads, err := store.list(context.Background())
	if err != nil || len(downloads) != 1 || downloads[0].ID != id {
		t.Fatalf("list() = %#v, %v; want preserved download", downloads, err)
	}
}

func TestGetMediaInfoUsesPersistentCacheWithoutTorrent(t *testing.T) {
	store, err := newDownloadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 40)
	magnet := "magnet:?xt=urn:btih:" + id
	if _, err := store.upsert(context.Background(), id, magnet, nil); err != nil {
		t.Fatal(err)
	}
	want := media.MediaInfo{
		VideoCodec:  "h264",
		AudioTracks: []media.AudioTrack{{Index: 0, Codec: "aac"}},
	}
	if err := store.saveMediaInfo(context.Background(), id, 3, want); err != nil {
		t.Fatal(err)
	}
	m := &Manager{downloads: store}

	got, err := m.GetMediaInfo(context.Background(), magnet, 3)
	if err != nil {
		t.Fatalf("GetMediaInfo() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetMediaInfo() = %#v, want %#v", got, want)
	}
}

func TestDownloadStoreExpiredIDs(t *testing.T) {
	store, err := newDownloadStore(t.TempDir())
	if err != nil {
		t.Fatalf("newDownloadStore() error = %v", err)
	}

	expiredID := strings.Repeat("b", 40)
	activeID := strings.Repeat("c", 40)
	expired, err := store.upsert(context.Background(), expiredID, "magnet:?xt=urn:btih:"+expiredID, nil)
	if err != nil {
		t.Fatalf("upsert(expired) error = %v", err)
	}
	active, err := store.upsert(context.Background(), activeID, "magnet:?xt=urn:btih:"+activeID, nil)
	if err != nil {
		t.Fatalf("upsert(active) error = %v", err)
	}

	expired.ExpiresAt = time.Now().Add(-time.Hour)
	if err := store.writeLockedForTest(expired); err != nil {
		t.Fatalf("write expired download: %v", err)
	}
	active.ExpiresAt = time.Now().Add(time.Hour)
	if err := store.writeLockedForTest(active); err != nil {
		t.Fatalf("write active download: %v", err)
	}

	ids, err := store.expiredIDs(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("expiredIDs() error = %v", err)
	}
	if len(ids) != 1 || ids[0] != expiredID {
		t.Fatalf("expiredIDs() = %#v, want [%s]", ids, expiredID)
	}
}

func (s *downloadStore) writeLockedForTest(download Download) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(download)
}
