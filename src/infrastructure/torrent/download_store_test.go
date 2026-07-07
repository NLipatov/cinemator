package torrent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cinemator/domain"
)

func TestDownloadStoreLifecycle(t *testing.T) {
	store, err := newDownloadStore(t.TempDir())
	if err != nil {
		t.Fatalf("newDownloadStore() error = %v", err)
	}

	id := strings.Repeat("a", 40)
	files := []domain.FileInfo{
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
	if download.Status != domain.DownloadStatusReady {
		t.Fatalf("download status = %q, want ready", download.Status)
	}
	if _, err := os.Stat(filepath.Join(store.root, id, downloadStoreDirName, "metadata.json")); err != nil {
		t.Fatalf("metadata not written inside download dir: %v", err)
	}

	downloads, err := store.list(context.Background())
	if err != nil {
		t.Fatalf("list() error = %v", err)
	}
	if len(downloads) != 1 || downloads[0].ID != id {
		t.Fatalf("list() = %#v, want one download with id %s", downloads, id)
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
		if _, err := cleanInfoHash(id); !errors.Is(err, domain.ErrBadDownloadID) {
			t.Fatalf("cleanInfoHash(%q) error = %v, want ErrBadDownloadID", id, err)
		}
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

func (s *downloadStore) writeLockedForTest(download domain.Download) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(download)
}
