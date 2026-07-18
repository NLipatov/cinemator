//go:build !unix

package torrent

import "os"

func openHlsAssetFile(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
