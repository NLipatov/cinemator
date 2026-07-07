package torrent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"cinemator/domain"
)

const (
	downloadStoreDirName = ".cinemator"
	downloadDefaultTTL   = 7 * 24 * time.Hour
)

type downloadStore struct {
	root string
	mu   sync.Mutex
}

func newDownloadStore(downloadRoot string) (*downloadStore, error) {
	if err := os.MkdirAll(downloadRoot, 0755); err != nil {
		return nil, err
	}
	return &downloadStore{root: downloadRoot}, nil
}

func (s *downloadStore) upsert(ctx context.Context, id, magnet string, files []domain.FileInfo) (domain.Download, error) {
	if err := ctx.Err(); err != nil {
		return domain.Download{}, err
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return domain.Download{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	download, err := s.readLocked(id)
	if err != nil && !errors.Is(err, domain.ErrDownloadNotFound) {
		return domain.Download{}, err
	}
	if errors.Is(err, domain.ErrDownloadNotFound) {
		download = domain.Download{
			ID:        id,
			CreatedAt: now,
			ExpiresAt: now.Add(downloadDefaultTTL),
		}
	}

	download.Magnet = magnet
	download.Files = cloneFiles(files)
	download.Size = totalFileSize(files)
	download.Title = downloadTitle(files, id)
	download.Status = domain.DownloadStatusReady
	download.UpdatedAt = now
	download.LastAccessedAt = now
	if download.ExpiresAt.IsZero() || download.ExpiresAt.Before(now) {
		download.ExpiresAt = now.Add(downloadDefaultTTL)
	}
	if err := s.writeLocked(download); err != nil {
		return domain.Download{}, err
	}
	return download, nil
}

func (s *downloadStore) list(ctx context.Context) ([]domain.Download, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	now := time.Now().UTC()
	downloads := make([]domain.Download, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := cleanInfoHash(entry.Name()); err != nil {
			continue
		}
		download, err := s.readLocked(entry.Name())
		if err != nil {
			if errors.Is(err, domain.ErrDownloadNotFound) {
				continue
			}
			return nil, err
		}
		if now.After(download.ExpiresAt) {
			download.Status = domain.DownloadStatusExpired
		} else if download.Status == domain.DownloadStatusExpired {
			download.Status = domain.DownloadStatusReady
		}
		downloads = append(downloads, download)
	}
	sort.SliceStable(downloads, func(i, j int) bool {
		left := downloads[i].LastAccessedAt
		right := downloads[j].LastAccessedAt
		if left.Equal(right) {
			return downloads[i].CreatedAt.After(downloads[j].CreatedAt)
		}
		return left.After(right)
	})
	return downloads, nil
}

func (s *downloadStore) expiredIDs(ctx context.Context, now time.Time) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var ids []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id, err := cleanInfoHash(entry.Name())
		if err != nil {
			continue
		}
		download, err := s.readLocked(id)
		if err != nil {
			if errors.Is(err, domain.ErrDownloadNotFound) {
				continue
			}
			return nil, err
		}
		if !download.ExpiresAt.IsZero() && now.After(download.ExpiresAt) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (s *downloadStore) touch(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	download, err := s.readLocked(id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	download.LastAccessedAt = now
	download.UpdatedAt = now
	if download.ExpiresAt.Before(now) {
		download.ExpiresAt = now.Add(downloadDefaultTTL)
	}
	download.Status = domain.DownloadStatusReady
	return s.writeLocked(download)
}

func (s *downloadStore) extend(ctx context.Context, id string, extension time.Duration) (domain.Download, error) {
	if err := ctx.Err(); err != nil {
		return domain.Download{}, err
	}
	if extension <= 0 {
		return domain.Download{}, fmt.Errorf("extension must be positive")
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return domain.Download{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	download, err := s.readLocked(id)
	if err != nil {
		return domain.Download{}, err
	}
	now := time.Now().UTC()
	base := download.ExpiresAt
	if base.Before(now) {
		base = now
	}
	download.ExpiresAt = base.Add(extension)
	download.UpdatedAt = now
	if now.After(download.ExpiresAt) {
		download.Status = domain.DownloadStatusExpired
	} else {
		download.Status = domain.DownloadStatusReady
	}
	if err := s.writeLocked(download); err != nil {
		return domain.Download{}, err
	}
	return download, nil
}

func (s *downloadStore) delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.RemoveAll(s.downloadDir(id)); err != nil {
		return err
	}
	return nil
}

func (s *downloadStore) readLocked(id string) (domain.Download, error) {
	data, err := os.ReadFile(s.metadataPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.Download{}, domain.ErrDownloadNotFound
		}
		return domain.Download{}, err
	}
	var download domain.Download
	if err := json.Unmarshal(data, &download); err != nil {
		return domain.Download{}, err
	}
	if download.ID == "" {
		download.ID = id
	}
	if download.Status == "" {
		download.Status = domain.DownloadStatusReady
	}
	return download, nil
}

func (s *downloadStore) writeLocked(download domain.Download) error {
	id, err := cleanInfoHash(download.ID)
	if err != nil {
		return err
	}
	dir := s.downloadDir(id)
	if err := os.MkdirAll(filepath.Join(dir, downloadStoreDirName), 0755); err != nil {
		return err
	}
	download.ID = id
	data, err := json.MarshalIndent(download, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	path := s.metadataPath(id)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *downloadStore) downloadDir(id string) string {
	return filepath.Join(s.root, id)
}

func (s *downloadStore) metadataPath(id string) string {
	return filepath.Join(s.downloadDir(id), downloadStoreDirName, "metadata.json")
}

func cleanInfoHash(id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) != 40 {
		return "", domain.ErrBadDownloadID
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", domain.ErrBadDownloadID
	}
	return id, nil
}

func cloneFiles(files []domain.FileInfo) []domain.FileInfo {
	if len(files) == 0 {
		return nil
	}
	out := make([]domain.FileInfo, len(files))
	copy(out, files)
	return out
}

func totalFileSize(files []domain.FileInfo) int64 {
	var total int64
	for _, file := range files {
		total += file.Size
	}
	return total
}

func downloadTitle(files []domain.FileInfo, id string) string {
	if len(files) == 0 {
		return "Torrent " + id[:8]
	}
	best := files[0]
	for _, file := range files[1:] {
		if file.Size > best.Size {
			best = file
		}
	}
	name := filepath.Base(best.Name)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "Torrent " + id[:8]
	}
	return name
}
