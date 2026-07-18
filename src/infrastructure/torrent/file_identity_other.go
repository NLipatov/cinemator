//go:build !unix

package torrent

import "os"

func platformFileLinkCount(os.FileInfo) uint64 { return 1 }
