package torrent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const cacheOwnerLockName = ".cinemator-owner.lock"

type cacheOwnership struct {
	files []*os.File
}

func (o *cacheOwnership) guardFiles() []*os.File {
	if o == nil {
		return nil
	}
	return append([]*os.File(nil), o.files...)
}

func acquireCacheOwnership(roots ...string) (*cacheOwnership, error) {
	unique := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		unique[filepath.Clean(abs)] = struct{}{}
	}
	ordered := make([]string, 0, len(unique))
	for root := range unique {
		ordered = append(ordered, root)
	}
	sort.Strings(ordered)

	ownership := &cacheOwnership{}
	for _, root := range ordered {
		file, err := openCacheOwnerLock(root)
		if err != nil {
			if file != nil {
				_ = file.Close()
			}
			_ = ownership.Close()
			return nil, fmt.Errorf("cache root %s already has an owner: %w", root, err)
		}
		ownership.files = append(ownership.files, file)
	}
	return ownership, nil
}

func openCacheOwnerLock(rootPath string) (*os.File, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	before, err := root.Lstat(cacheOwnerLockName)
	if err == nil && (!before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Sys() != nil && fileLinkCount(before) != 1) {
		return nil, fmt.Errorf("cache owner lock is not a private regular file: %s", filepath.Join(rootPath, cacheOwnerLockName))
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	file, err := openCacheOwnerFile(root)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Sys() != nil && fileLinkCount(info) != 1 {
		_ = file.Close()
		return nil, errors.Join(err, errors.New("cache owner lock has unexpected identity"))
	}
	if err := lockCacheOwner(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	after, err := root.Lstat(cacheOwnerLockName)
	if err != nil || !os.SameFile(info, after) {
		_ = file.Close()
		return nil, errors.Join(err, errors.New("cache owner lock changed while acquiring ownership"))
	}
	return file, nil
}

func (o *cacheOwnership) Close() error {
	if o == nil {
		return nil
	}
	var result error
	for index := len(o.files) - 1; index >= 0; index-- {
		file := o.files[index]
		// Do not explicitly LOCK_UN. Owned children inherit this open-file
		// description, so a plain close keeps startup fenced until the last old
		// child or descendant exits after an abrupt or timed-out shutdown.
		result = errors.Join(result, file.Close())
	}
	o.files = nil
	return result
}
