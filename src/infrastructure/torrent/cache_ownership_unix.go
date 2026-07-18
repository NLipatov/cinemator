//go:build unix

package torrent

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockCacheOwner(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
