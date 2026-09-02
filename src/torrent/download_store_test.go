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
	if download.Status != DownloadStatusAwaitingSelection {
		t.Fatalf("download status = %q, want awaiting selection", download.Status)
	}
	if !download.ExpiresAt.IsZero() {
		t.Fatalf("download expiry = %v, want zero before HLS is ready", download.ExpiresAt)
	}
	preparing, shouldStart, err := store.beginPreparation(context.Background(), id, 1)
	if err != nil {
		t.Fatalf("beginPreparation() error = %v", err)
	}
	if !shouldStart || preparing.Status != DownloadStatusPreparing || preparing.SelectedFileIndex == nil || *preparing.SelectedFileIndex != 1 {
		t.Fatalf("beginPreparation() = %#v, %v", preparing, shouldStart)
	}
	completedAt := time.Now().UTC().Truncate(time.Second)
	if err := store.finishPreparation(context.Background(), id, 1, completedAt); err != nil {
		t.Fatalf("finishPreparation() error = %v", err)
	}
	downloads, err := store.list(context.Background())
	if err != nil {
		t.Fatalf("list() after preparation error = %v", err)
	}
	if len(downloads) != 1 || downloads[0].Status != DownloadStatusReady {
		t.Fatalf("list() after preparation = %#v, want one ready download", downloads)
	}
	download = downloads[0]
	if !download.ReadyAt.Equal(completedAt) {
		t.Fatalf("ready at = %v, want %v", download.ReadyAt, completedAt)
	}
	if want := completedAt.Add(downloadDefaultTTL); !download.ExpiresAt.Equal(want) {
		t.Fatalf("expiry = %v, want %v", download.ExpiresAt, want)
	}
	if _, err := os.Stat(filepath.Join(store.root, id, downloadStoreDirName, "metadata.json")); err != nil {
		t.Fatalf("metadata not written inside download dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.root, id, "payload.bin"), []byte("payload"), 0644); err != nil {
		t.Fatalf("write payload file: %v", err)
	}

	downloads, err = store.list(context.Background())
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

func TestDownloadStorePreparationFailureCanBeRetried(t *testing.T) {
	store, err := newDownloadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("d", 40)
	files := []FileInfo{{Index: 3, Name: "feature.mkv", Size: 10}}
	if _, err := store.upsert(context.Background(), id, "magnet:?xt=urn:btih:"+id, files); err != nil {
		t.Fatal(err)
	}
	if _, shouldStart, err := store.beginPreparation(context.Background(), id, 3); err != nil || !shouldStart {
		t.Fatalf("beginPreparation() = %v, %v", shouldStart, err)
	}
	if err := store.failPreparation(context.Background(), id, 3, errors.New("ffmpeg failed")); err != nil {
		t.Fatal(err)
	}
	downloads, err := store.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := downloads[0]; got.Status != DownloadStatusFailed || got.PreparationErr != "ffmpeg failed" {
		t.Fatalf("failed download = %#v", got)
	}
	if _, shouldStart, err := store.beginPreparation(context.Background(), id, 3); err != nil || !shouldStart {
		t.Fatalf("retry beginPreparation() = %v, %v", shouldStart, err)
	}
}

func TestFinishPreparationExpiresLateStaleOutput(t *testing.T) {
	store, err := newDownloadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 40)
	files := []FileInfo{
		{Index: 0, Name: "episode-1.mkv", Size: 10},
		{Index: 1, Name: "episode-2.mkv", Size: 10},
	}
	if _, err := store.upsert(context.Background(), id, "magnet:?xt=urn:btih:"+id, files); err != nil {
		t.Fatal(err)
	}
	if _, shouldStart, err := store.beginPreparation(context.Background(), id, 0); err != nil || !shouldStart {
		t.Fatalf("begin first preparation = %v, %v", shouldStart, err)
	}
	if _, shouldStart, err := store.beginPreparation(context.Background(), id, 1); err != nil || !shouldStart {
		t.Fatalf("begin replacement preparation = %v, %v", shouldStart, err)
	}
	if err := store.failPreparation(context.Background(), id, 1, errors.New("replacement failed")); err != nil {
		t.Fatal(err)
	}

	completedAt := time.Now().UTC().Truncate(time.Second)
	if err := store.finishPreparation(context.Background(), id, 0, completedAt); err != nil {
		t.Fatal(err)
	}
	downloads, err := store.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := downloads[0]
	if got.Status != DownloadStatusFailed || got.SelectedFileIndex == nil || *got.SelectedFileIndex != 1 {
		t.Fatalf("late completion replaced failed selection: %#v", got)
	}
	if want := completedAt.Add(downloadDefaultTTL); !got.ExpiresAt.Equal(want) {
		t.Fatalf("expiry = %v, want %v", got.ExpiresAt, want)
	}
}

func TestBeginPreparationClearsPreviousExpiry(t *testing.T) {
	store, err := newDownloadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 40)
	files := []FileInfo{{Index: 0, Name: "feature.mkv", Size: 10}}
	if _, err := store.upsert(context.Background(), id, "magnet:?xt=urn:btih:"+id, files); err != nil {
		t.Fatal(err)
	}
	if _, shouldStart, err := store.beginPreparation(context.Background(), id, 0); err != nil || !shouldStart {
		t.Fatalf("beginPreparation() = %v, %v", shouldStart, err)
	}
	completedAt := time.Now().UTC().Add(-downloadDefaultTTL - time.Hour)
	if err := store.finishPreparation(context.Background(), id, 0, completedAt); err != nil {
		t.Fatal(err)
	}

	preparing, shouldStart, err := store.beginPreparation(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !shouldStart || preparing.Status != DownloadStatusPreparing {
		t.Fatalf("beginPreparation() = %#v, %v; want preparing, true", preparing, shouldStart)
	}
	if !preparing.ExpiresAt.IsZero() {
		t.Fatalf("preparing expiry = %v, want zero", preparing.ExpiresAt)
	}

	downloads, err := store.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(downloads) != 1 || downloads[0].Status != DownloadStatusPreparing {
		t.Fatalf("list() = %#v, want one preparing download", downloads)
	}
	expired, err := store.expiredIDs(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 0 {
		t.Fatalf("expiredIDs() = %v, want none", expired)
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

func TestLegacyMultiVideoMigrationClearsObsoleteExpiry(t *testing.T) {
	store, err := newDownloadStore(t.TempDir())
	if err != nil {
		t.Fatalf("newDownloadStore() error = %v", err)
	}

	id := strings.Repeat("d", 40)
	files := []FileInfo{
		{Index: 0, Name: "episode-1.mkv"},
		{Index: 1, Name: "episode-2.mkv"},
	}
	download, err := store.upsert(context.Background(), id, "magnet:?xt=urn:btih:"+id, files)
	if err != nil {
		t.Fatalf("upsert() error = %v", err)
	}
	download.Status = DownloadStatus("streaming")
	download.ExpiresAt = time.Now().Add(-time.Hour)
	if err := store.writeLockedForTest(download); err != nil {
		t.Fatalf("write legacy download: %v", err)
	}

	downloads, err := store.list(context.Background())
	if err != nil {
		t.Fatalf("list() error = %v", err)
	}
	if len(downloads) != 1 || downloads[0].Status != DownloadStatusAwaitingSelection {
		t.Fatalf("downloads = %#v, want one awaiting-selection record", downloads)
	}
	if !downloads[0].ExpiresAt.IsZero() {
		t.Fatalf("expiresAt = %v, want zero after migration", downloads[0].ExpiresAt)
	}
	ids, err := store.expiredIDs(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("expiredIDs() error = %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expiredIDs() = %#v, want none", ids)
	}
}

func (s *downloadStore) writeLockedForTest(download Download) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(download)
}
