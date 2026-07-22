package torrent

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cinemator/application"
)

var errHlsAssetsBusy = errors.New("HLS assets are still in use")

// hlsAssetStore is the only owner allowed to open or remove published HLS
// files. Its lock makes open-and-lease linearizable with retirement and unlink.
type hlsAssetStore struct {
	path    string
	root    *os.Root
	mu      sync.Mutex
	files   map[string]*hlsAssetState
	retired map[string]struct{}
}

type hlsAssetState struct {
	readers  int
	retiring bool
	identity os.FileInfo
}

func newHlsAssetStore(root string) (*hlsAssetStore, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	path := filepath.Clean(abs)
	rootHandle, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &hlsAssetStore{
		path:    path,
		root:    rootHandle,
		files:   make(map[string]*hlsAssetState),
		retired: make(map[string]struct{}),
	}, nil
}

func (s *hlsAssetStore) Open(path string) (application.HlsAsset, error) {
	path, name, err := s.managedPath(path)
	if err != nil {
		return application.HlsAsset{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pathRetired(path) {
		return application.HlsAsset{}, fs.ErrNotExist
	}
	state := s.files[path]
	if state != nil && state.retiring {
		return application.HlsAsset{}, fs.ErrNotExist
	}
	linkInfo, err := s.root.Lstat(name)
	if err != nil {
		return application.HlsAsset{}, err
	}
	if err := validateHlsAsset(path, linkInfo); err != nil {
		return application.HlsAsset{}, err
	}

	file, err := openHlsAssetFile(s.root, name)
	if err != nil {
		return application.HlsAsset{}, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return application.HlsAsset{}, err
	}
	if err := validateHlsAsset(path, info); err != nil {
		_ = file.Close()
		return application.HlsAsset{}, err
	}
	if !os.SameFile(linkInfo, info) {
		_ = file.Close()
		return application.HlsAsset{}, fmt.Errorf("HLS asset changed while opening: %s", path)
	}
	state, err = s.bindState(path, info)
	if err != nil {
		_ = file.Close()
		return application.HlsAsset{}, err
	}
	state.readers++
	return application.HlsAsset{
		ReadSeekCloser: &leasedHlsFile{File: file, store: s, path: path, name: name, state: state},
		ModTime:        info.ModTime(),
	}, nil
}

func (s *hlsAssetStore) Touch(path string, minimumAge time.Duration) bool {
	path, name, err := s.managedPath(path)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pathRetired(path) {
		return false
	}
	info, err := s.root.Lstat(name)
	if err != nil || validateHlsAsset(path, info) != nil {
		return false
	}
	if time.Since(info.ModTime()) >= minimumAge {
		now := time.Now()
		_ = s.root.Chtimes(name, now, now)
	}
	return true
}

// TryEvict removes a file only when it has no active readers. A busy file stays
// named, accounted and openable; admission can fail instead of hiding blocks.
func (s *hlsAssetStore) TryEvict(path string) (bool, error) {
	return s.remove(path, false)
}

// CanEvict is used while the presentation generation lock blocks new opens.
// A true result therefore remains valid until the caller invokes TryEvict.
func (s *hlsAssetStore) CanEvict(path string) (bool, error) {
	path, name, err := s.managedPath(path)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateForExisting(path, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return state.readers == 0 && !state.retiring, nil
}

func (s *hlsAssetStore) hasReaders() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, state := range s.files {
		if state.readers != 0 {
			return true
		}
	}
	return false
}

func (s *hlsAssetStore) Close() error {
	return s.root.Close()
}

// RetireTree prevents new opens before enumerating the tree. Files with active
// readers are unlinked by their final Close; the directory remains visible
// until then.
func (s *hlsAssetStore) RetireTree(root string) error {
	root, name, err := s.managedPath(root)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.retired[root] = struct{}{}
	s.mu.Unlock()

	var paths []string
	walkErr := fs.WalkDir(s.root.FS(), filepath.ToSlash(name), func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			paths = append(paths, filepath.Join(s.path, filepath.FromSlash(path)))
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		return walkErr
	}
	busy := false
	for _, path := range paths {
		removed, err := s.remove(path, true)
		if err != nil {
			return err
		}
		busy = busy || !removed
	}
	if !busy {
		if err := s.root.RemoveAll(name); err != nil {
			return err
		}
		s.mu.Lock()
		delete(s.retired, root)
		s.mu.Unlock()
	}
	if busy {
		return errHlsAssetsBusy
	}
	return nil
}

func (s *hlsAssetStore) ResetTree(root string) error {
	root, name, err := s.managedPath(root)
	if err != nil {
		return err
	}
	if err := s.RetireTree(root); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s.mu.Lock()
	delete(s.retired, root)
	s.mu.Unlock()
	return s.root.MkdirAll(name, 0755)
}

func (s *hlsAssetStore) remove(path string, retireBusy bool) (bool, error) {
	path, name, err := s.managedPath(path)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateForExisting(path, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	if state.readers != 0 {
		if retireBusy {
			state.retiring = true
		}
		return false, nil
	}
	state.retiring = true
	if err := s.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		state.retiring = false
		return false, err
	}
	delete(s.files, path)
	return true, nil
}

func (s *hlsAssetStore) stateForExisting(path, name string) (*hlsAssetState, error) {
	info, err := s.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("HLS asset is a symbolic link: %s", path)
	}
	if err := validateHlsAsset(path, info); err != nil {
		return nil, err
	}
	return s.bindState(path, info)
}

func validateHlsAsset(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("HLS asset is not a regular file: %s", path)
	}
	links, verified := fileLinkCount(path, info)
	if !verified || links != 1 {
		return fmt.Errorf("HLS asset has unexpected link count: %s", path)
	}
	return nil
}

func (s *hlsAssetStore) bindState(path string, info os.FileInfo) (*hlsAssetState, error) {
	state := s.files[path]
	if state == nil {
		state = &hlsAssetState{identity: info}
		s.files[path] = state
	} else if !os.SameFile(state.identity, info) {
		if state.readers != 0 {
			return nil, fmt.Errorf("HLS asset changed while leased: %s", path)
		}
		state = &hlsAssetState{identity: info}
		s.files[path] = state
	}
	return state, nil
}

func (s *hlsAssetStore) managedPath(path string) (string, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	abs = filepath.Clean(abs)
	if abs != s.path && !strings.HasPrefix(abs, s.path+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("path is outside HLS cache: %s", path)
	}
	name, err := filepath.Rel(s.path, abs)
	if err != nil {
		return "", "", err
	}
	return abs, name, nil
}

func (s *hlsAssetStore) pathRetired(path string) bool {
	for root := range s.retired {
		if path == root || strings.HasPrefix(path, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func (s *hlsAssetStore) release(path, name string, state *hlsAssetState, closeErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if closeErr != nil {
		log.Printf("HLS asset close is ambiguous; keeping file accounted: path=%s err=%v", path, closeErr)
		return
	}
	if state.readers <= 0 {
		panic("released HLS asset without a reader lease")
	}
	state.readers--
	if state.readers != 0 || !state.retiring {
		return
	}
	if err := s.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("failed to remove retired HLS asset: path=%s err=%v", path, err)
		return
	}
	if s.files[path] == state {
		delete(s.files, path)
	}
	s.pruneRetiredTrees(path)
}

func (s *hlsAssetStore) pruneRetiredTrees(path string) {
	for root := range s.retired {
		if path != root && !strings.HasPrefix(path, root+string(os.PathSeparator)) {
			continue
		}
		busy := false
		for assetPath, state := range s.files {
			if (assetPath == root || strings.HasPrefix(assetPath, root+string(os.PathSeparator))) && state.readers != 0 {
				busy = true
				break
			}
		}
		if busy {
			continue
		}
		_, name, err := s.managedPath(root)
		if err != nil {
			log.Printf("failed to resolve retired HLS tree: path=%s err=%v", root, err)
			continue
		}
		if err := s.root.RemoveAll(name); err != nil {
			log.Printf("failed to remove retired HLS tree: path=%s err=%v", root, err)
			continue
		}
		for assetPath := range s.files {
			if assetPath == root || strings.HasPrefix(assetPath, root+string(os.PathSeparator)) {
				delete(s.files, assetPath)
			}
		}
		delete(s.retired, root)
	}
}

type leasedHlsFile struct {
	*os.File
	store *hlsAssetStore
	path  string
	name  string
	state *hlsAssetState
	once  sync.Once
	err   error
}

func (f *leasedHlsFile) Close() error {
	f.once.Do(func() {
		f.err = f.File.Close()
		// Close must complete before the lease can reach zero.
		f.store.release(f.path, f.name, f.state, f.err)
	})
	return f.err
}

func fileLinkCount(path string, info os.FileInfo) (uint64, bool) {
	return platformFileLinkCount(path, info)
}

var _ application.ReadSeekCloser = (*leasedHlsFile)(nil)
