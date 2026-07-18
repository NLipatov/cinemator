//go:build unix

package torrent

import (
	"fmt"
	"math"
	"os"
	"syscall"
)

func filesystemAvailability(path string) (diskAvailability, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return diskAvailability{}, err
	}
	blockSize := uint64(stat.Bsize)
	if blockSize != 0 && stat.Bavail > math.MaxUint64/blockSize {
		return diskAvailability{}, fmt.Errorf("filesystem availability overflows for %s", path)
	}
	return diskAvailability{bytes: stat.Bavail * blockSize, inodes: stat.Ffree}, nil
}

func filesystemDevice(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("cannot identify filesystem for %s", path)
	}
	return fmt.Sprint(stat.Dev), nil
}
