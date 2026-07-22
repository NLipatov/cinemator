//go:build !unix && !windows

package torrent

import "os"

func platformFileLinkCount(string, os.FileInfo) (uint64, bool) { return 0, false }
