//go:build !unix

package torrent

import (
	"errors"
	"os"
)

func lockCacheOwner(*os.File) error {
	return errors.New("cache ownership is unsupported on this platform")
}

func openCacheOwnerFile(root *os.Root) (*os.File, error) {
	return root.OpenFile(cacheOwnerLockName, os.O_CREATE|os.O_RDWR, 0600)
}
