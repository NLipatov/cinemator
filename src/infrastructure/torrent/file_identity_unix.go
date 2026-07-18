//go:build unix

package torrent

import (
	"os"
	"syscall"
)

func platformFileLinkCount(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 1
	}
	return uint64(stat.Nlink)
}
