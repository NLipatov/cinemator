package torrent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"cinemator/media"
)

const (
	downloadStoreDirName = ".cinemator"
	downloadDefaultTTL   = 7 * 24 * time.Hour
)

type downloadStore struct {
	root string
	mu   sync.Mutex
}

type cachedMediaInfo struct {
	VideoCodec  string                `json:"videoCodec"`
	NeedFilter  bool                  `json:"needFilter"`
	AudioTracks []media.AudioTrack    `json:"audioTracks"`
	Subtitles   []media.SubtitleTrack `json:"subtitles"`
}

func newDownloadStore(downloadRoot string) (*downloadStore, error) {
	if err := os.MkdirAll(downloadRoot, 0755); err != nil {
		return nil, err
	}
	return &downloadStore{root: downloadRoot}, nil
}

func (s *downloadStore) upsert(ctx context.Context, id, magnet string, files []FileInfo) (Download, error) {
	if err := ctx.Err(); err != nil {
		return Download{}, err
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return Download{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	download, err := s.readLocked(id)
	if err != nil && !errors.Is(err, ErrDownloadNotFound) {
		return Download{}, err
	}
	if errors.Is(err, ErrDownloadNotFound) {
		download = Download{
			ID:        id,
			Status:    DownloadStatusAwaitingSelection,
			CreatedAt: now,
		}
	}

	download.Magnet = magnet
	download.Files = cloneFiles(files)
	download.Size = totalFileSize(files)
	download.Title = downloadTitle(files, id)
	download.UpdatedAt = now
	download.LastAccessedAt = now
	if !download.ExpiresAt.IsZero() && download.ExpiresAt.Before(now) {
		download.Status = DownloadStatusAwaitingSelection
		download.SelectedFileIndex = nil
		download.ReadyAt = time.Time{}
		download.ExpiresAt = time.Time{}
		download.PreparationErr = ""
	}
	if download.SelectedFileIndex != nil && !hasFileIndex(download.Files, *download.SelectedFileIndex) {
		download.Status = DownloadStatusAwaitingSelection
		download.SelectedFileIndex = nil
		download.ReadyAt = time.Time{}
		download.ExpiresAt = time.Time{}
		download.PreparationErr = ""
	}
	if err := s.writeLocked(download); err != nil {
		return Download{}, err
	}
	download.DiskSize = s.diskSizeLocked(id)
	return download, nil
}

func (s *downloadStore) list(ctx context.Context) ([]Download, error) {
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
	downloads := make([]Download, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := cleanInfoHash(entry.Name()); err != nil {
			continue
		}
		download, err := s.readLocked(entry.Name())
		if err != nil {
			if errors.Is(err, ErrDownloadNotFound) {
				continue
			}
			return nil, err
		}
		if !download.ExpiresAt.IsZero() && now.After(download.ExpiresAt) {
			download.Status = DownloadStatusExpired
		}
		download.DiskSize = s.diskSizeLocked(entry.Name())
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
			if errors.Is(err, ErrDownloadNotFound) {
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
	return s.writeLocked(download)
}

func (s *downloadStore) extend(ctx context.Context, id string, extension time.Duration) (Download, error) {
	if err := ctx.Err(); err != nil {
		return Download{}, err
	}
	if extension <= 0 {
		return Download{}, fmt.Errorf("extension must be positive")
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return Download{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	download, err := s.readLocked(id)
	if err != nil {
		return Download{}, err
	}
	if download.Status != DownloadStatusReady || download.ExpiresAt.IsZero() {
		return Download{}, ErrDownloadNotReady
	}
	now := time.Now().UTC()
	base := download.ExpiresAt
	if base.Before(now) {
		base = now
	}
	download.ExpiresAt = base.Add(extension)
	download.UpdatedAt = now
	download.Status = DownloadStatusReady
	if err := s.writeLocked(download); err != nil {
		return Download{}, err
	}
	download.DiskSize = s.diskSizeLocked(id)
	return download, nil
}

func (s *downloadStore) beginPreparation(ctx context.Context, id string, fileIndex int) (Download, bool, error) {
	if err := ctx.Err(); err != nil {
		return Download{}, false, err
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return Download{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	download, err := s.readLocked(id)
	if err != nil {
		return Download{}, false, err
	}
	if !hasFileIndex(download.Files, fileIndex) {
		return Download{}, false, fmt.Errorf("bad file index")
	}
	if download.SelectedFileIndex != nil && *download.SelectedFileIndex == fileIndex &&
		download.Status == DownloadStatusPreparing {
		return download, false, nil
	}

	now := time.Now().UTC()
	download.SelectedFileIndex = intPointer(fileIndex)
	download.Status = DownloadStatusPreparing
	download.ReadyAt = time.Time{}
	download.ExpiresAt = time.Time{}
	download.PreparationErr = ""
	download.UpdatedAt = now
	download.LastAccessedAt = now
	if err := s.writeLocked(download); err != nil {
		return Download{}, false, err
	}
	return download, true, nil
}

func (s *downloadStore) selectPrepared(ctx context.Context, id string, fileIndex int, selectedAt time.Time) error {
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
	if !hasFileIndex(download.Files, fileIndex) {
		return fmt.Errorf("bad file index")
	}
	if download.Status == DownloadStatusReady && download.SelectedFileIndex != nil && *download.SelectedFileIndex == fileIndex {
		return nil
	}
	return s.writeReadyLocked(download, fileIndex, selectedAt)
}

func (s *downloadStore) finishPreparation(ctx context.Context, id string, fileIndex int, completedAt time.Time) error {
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
	if !hasFileIndex(download.Files, fileIndex) {
		return fmt.Errorf("bad file index")
	}
	if download.SelectedFileIndex != nil && *download.SelectedFileIndex != fileIndex {
		return nil
	}
	if download.Status == DownloadStatusReady && download.SelectedFileIndex != nil && *download.SelectedFileIndex == fileIndex {
		return nil
	}
	return s.writeReadyLocked(download, fileIndex, completedAt)
}

func (s *downloadStore) writeReadyLocked(download Download, fileIndex int, completedAt time.Time) error {
	completedAt = completedAt.UTC()
	download.SelectedFileIndex = intPointer(fileIndex)
	download.Status = DownloadStatusReady
	download.ReadyAt = completedAt
	download.ExpiresAt = completedAt.Add(downloadDefaultTTL)
	download.PreparationErr = ""
	download.UpdatedAt = completedAt
	download.LastAccessedAt = completedAt
	return s.writeLocked(download)
}

func (s *downloadStore) failPreparation(ctx context.Context, id string, fileIndex int, preparationErr error) error {
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
	if download.SelectedFileIndex == nil || *download.SelectedFileIndex != fileIndex || download.Status != DownloadStatusPreparing {
		return nil
	}
	download.Status = DownloadStatusFailed
	download.PreparationErr = preparationErr.Error()
	download.UpdatedAt = time.Now().UTC()
	return s.writeLocked(download)
}

func (s *downloadStore) ensureFailedHLSExpiry(ctx context.Context, id string, failedAt time.Time) error {
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
	if download.Status != DownloadStatusFailed || !download.ExpiresAt.IsZero() {
		return nil
	}
	failedAt = failedAt.UTC()
	download.ExpiresAt = failedAt.Add(downloadDefaultTTL)
	download.UpdatedAt = failedAt
	return s.writeLocked(download)
}

func (s *downloadStore) payloadDisposable(ctx context.Context, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	download, err := s.readLocked(id)
	if err != nil {
		return false, err
	}
	return download.Status == DownloadStatusReady || download.Status == DownloadStatusFailed, nil
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

func (s *downloadStore) deletePayload(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.downloadDir(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.Name() == downloadStoreDirName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.downloadDir(id), entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (s *downloadStore) loadMediaInfo(ctx context.Context, id string, fileIndex int) (media.MediaInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return media.MediaInfo{}, false, err
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return media.MediaInfo{}, false, err
	}
	if fileIndex < 0 {
		return media.MediaInfo{}, false, fmt.Errorf("bad file index")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.mediaInfoPath(id, fileIndex))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return media.MediaInfo{}, false, nil
		}
		return media.MediaInfo{}, false, err
	}
	var cached cachedMediaInfo
	if err := json.Unmarshal(data, &cached); err != nil {
		return media.MediaInfo{}, false, err
	}
	return media.MediaInfo{
		VideoCodec:  cached.VideoCodec,
		NeedFilter:  cached.NeedFilter,
		AudioTracks: cached.AudioTracks,
		Subtitles:   cached.Subtitles,
	}, true, nil
}

func (s *downloadStore) saveMediaInfo(ctx context.Context, id string, fileIndex int, info media.MediaInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return err
	}
	if fileIndex < 0 {
		return fmt.Errorf("bad file index")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.downloadDir(id), downloadStoreDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cachedMediaInfo{
		VideoCodec:  info.VideoCodec,
		NeedFilter:  info.NeedFilter,
		AudioTracks: info.AudioTracks,
		Subtitles:   info.Subtitles,
	}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	path := s.mediaInfoPath(id, fileIndex)
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

func (s *downloadStore) readLocked(id string) (Download, error) {
	data, err := os.ReadFile(s.metadataPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Download{}, ErrDownloadNotFound
		}
		return Download{}, err
	}
	var download Download
	if err := json.Unmarshal(data, &download); err != nil {
		return Download{}, err
	}
	if download.ID == "" {
		download.ID = id
	}
	if download.Status == "" {
		download.Status = DownloadStatusAwaitingSelection
	}
	if download.SelectedFileIndex == nil && download.ReadyAt.IsZero() &&
		(download.Status == DownloadStatusReady || download.Status == DownloadStatus("streaming")) {
		download.Status = DownloadStatusAwaitingSelection
	}
	return download, nil
}

func (s *downloadStore) writeLocked(download Download) error {
	id, err := cleanInfoHash(download.ID)
	if err != nil {
		return err
	}
	dir := s.downloadDir(id)
	if err := os.MkdirAll(filepath.Join(dir, downloadStoreDirName), 0755); err != nil {
		return err
	}
	download.ID = id
	download.DiskSize = 0
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

func (s *downloadStore) mediaInfoPath(id string, fileIndex int) string {
	return filepath.Join(s.downloadDir(id), downloadStoreDirName, fmt.Sprintf("media-%d.json", fileIndex))
}

func (s *downloadStore) diskSizeLocked(id string) int64 {
	var total int64
	_ = filepath.WalkDir(s.downloadDir(id), func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == downloadStoreDirName {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		total += allocatedFileSize(info)
		return nil
	})
	return total
}

func allocatedFileSize(info fs.FileInfo) int64 {
	if blocks, ok := fileBlocks(info); ok && blocks > 0 {
		return blocks * 512
	}
	return info.Size()
}

func fileBlocks(info fs.FileInfo) (int64, bool) {
	sys := info.Sys()
	if sys == nil {
		return 0, false
	}
	value := reflect.ValueOf(sys)
	if value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return 0, false
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return 0, false
	}
	field := value.FieldByName("Blocks")
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return field.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		blocks := field.Uint()
		if blocks > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(blocks), true
	default:
		return 0, false
	}
}

func cleanInfoHash(id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) != 40 {
		return "", ErrBadDownloadID
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", ErrBadDownloadID
	}
	return id, nil
}

func cloneFiles(files []FileInfo) []FileInfo {
	if len(files) == 0 {
		return nil
	}
	out := make([]FileInfo, len(files))
	copy(out, files)
	return out
}

func totalFileSize(files []FileInfo) int64 {
	var total int64
	for _, file := range files {
		total += file.Size
	}
	return total
}

func downloadTitle(files []FileInfo, id string) string {
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

func hasFileIndex(files []FileInfo, index int) bool {
	for _, file := range files {
		if file.Index == index {
			return true
		}
	}
	return false
}

func intPointer(value int) *int {
	return &value
}
