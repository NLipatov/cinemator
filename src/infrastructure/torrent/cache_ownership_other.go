//go:build !unix

package torrent

import (
	"errors"
	"os"
)

func lockCacheOwner(*os.File) error {
	return errors.New("cache ownership is unsupported on this platform")
}
