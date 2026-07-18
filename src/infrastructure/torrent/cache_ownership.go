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
		lockPath := filepath.Join(root, cacheOwnerLockName)
		if info, err := os.Lstat(lockPath); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Sys() != nil && fileLinkCount(info) != 1) {
			_ = ownership.Close()
			return nil, fmt.Errorf("cache owner lock is not a private regular file: %s", lockPath)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = ownership.Close()
			return nil, err
		}
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
		if err == nil {
			info, statErr := file.Stat()
			if statErr != nil || !info.Mode().IsRegular() || info.Sys() != nil && fileLinkCount(info) != 1 {
				err = errors.Join(statErr, errors.New("cache owner lock has unexpected identity"))
			} else {
				err = lockCacheOwner(file)
			}
		}
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
