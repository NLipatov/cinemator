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

	"cinemator/domain"
)

const (
	downloadStoreDirName   = ".cinemator"
	downloadDefaultTTL     = 7 * 24 * time.Hour
	downloadTouchInterval  = time.Minute
	mediaDescriptorVersion = 1
)

type storedMediaDescriptor struct {
	Version         int              `json:"version"`
	Info            domain.MediaInfo `json:"info"`
	VideoTrackIndex int              `json:"videoTrackIndex"`
	NeedFilter      bool             `json:"needFilter"`
}

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

// discardLegacyPayloads removes the pre-piece-cache file layout while preserving
// saved download metadata. Torrent payload is a disposable cache and will be
// fetched into the bounded content-addressed cache when requested again.
func (s *downloadStore) discardLegacyPayloads() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := cleanInfoHash(entry.Name()); err != nil {
			continue
		}
		download, err := s.readLocked(entry.Name())
		if err != nil {
			// A hash-shaped directory is not necessarily owned by Cinemator. Only
			// migrate directories that contain valid Cinemator metadata.
			continue
		}
		dir := s.downloadDir(entry.Name())
		root, err := os.OpenRoot(dir)
		if err != nil {
			return err
		}
		removeErr := func() error {
			defer root.Close()
			for _, file := range download.Files {
				rel := filepath.Clean(filepath.FromSlash(file.Name))
				if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
					continue
				}
				if rel == downloadStoreDirName || strings.HasPrefix(rel, downloadStoreDirName+string(os.PathSeparator)) {
					continue
				}
				for _, suffix := range []string{"", ".part"} {
					payload := rel + suffix
					symlinked, err := hasSymlinkParent(root, payload)
					if err != nil {
						return err
					}
					if symlinked {
						continue
					}
					if err := root.RemoveAll(payload); err != nil {
						return err
					}
					removeEmptyParents(root, filepath.Dir(payload))
				}
			}
			return nil
		}()
		if removeErr != nil {
			return removeErr
		}
	}
	return nil
}

func hasSymlinkParent(root *os.Root, path string) (bool, error) {
	parent := filepath.Dir(filepath.Clean(path))
	if parent == "." {
		return false, nil
	}
	current := ""
	for _, component := range strings.Split(parent, string(os.PathSeparator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true, nil
		}
		if !info.IsDir() {
			return true, nil
		}
	}
	return false, nil
}

func removeEmptyParents(root *os.Root, path string) {
	for path = filepath.Clean(path); path != "."; path = filepath.Dir(path) {
		if err := root.Remove(path); err != nil {
			return
		}
	}
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
	download.DiskSize = s.diskSizeLocked(id)
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

func (s *downloadStore) touch(ctx context.Context, id string) (bool, error) {
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
	now := time.Now().UTC()
	if download.Status == domain.DownloadStatusReady && !download.ExpiresAt.Before(now) && now.Sub(download.LastAccessedAt) < downloadTouchInterval {
		return false, nil
	}
	download.LastAccessedAt = now
	download.UpdatedAt = now
	if download.ExpiresAt.Before(now) {
		download.ExpiresAt = now.Add(downloadDefaultTTL)
	}
	download.Status = domain.DownloadStatusReady
	if err := s.writeLocked(download); err != nil {
		return false, err
	}
	return true, nil
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
	download.DiskSize = s.diskSizeLocked(id)
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

func (s *downloadStore) readMediaInfo(ctx context.Context, id string, index int) (domain.MediaInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.MediaInfo{}, false, err
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return domain.MediaInfo{}, false, err
	}
	if index < 0 {
		return domain.MediaInfo{}, false, fmt.Errorf("invalid media index")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.mediaInfoPath(id, index))
	if errors.Is(err, os.ErrNotExist) {
		return domain.MediaInfo{}, false, nil
	}
	if err != nil {
		return domain.MediaInfo{}, false, err
	}
	var descriptor storedMediaDescriptor
	if err := json.Unmarshal(data, &descriptor); err != nil {
		return domain.MediaInfo{}, false, err
	}
	if descriptor.Version != mediaDescriptorVersion || descriptor.Info.VideoCodec == "" {
		return domain.MediaInfo{}, false, nil
	}
	descriptor.Info.VideoTrackIndex = descriptor.VideoTrackIndex
	descriptor.Info.NeedFilter = descriptor.NeedFilter
	return descriptor.Info, true, nil
}

func (s *downloadStore) writeMediaInfo(ctx context.Context, id string, index int, info domain.MediaInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := cleanInfoHash(id)
	if err != nil {
		return err
	}
	if index < 0 || info.VideoCodec == "" {
		return fmt.Errorf("invalid media descriptor")
	}
	descriptor := storedMediaDescriptor{
		Version:         mediaDescriptorVersion,
		Info:            info,
		VideoTrackIndex: info.VideoTrackIndex,
		NeedFilter:      info.NeedFilter,
	}
	data, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.downloadDir(id), downloadStoreDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := s.mediaInfoPath(id, index)
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

func (s *downloadStore) mediaInfoPath(id string, index int) string {
	return filepath.Join(s.downloadDir(id), downloadStoreDirName, fmt.Sprintf("media-%d.json", index))
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
