package torrent

import "errors"

var ErrUnsupportedMagnetVersion = errors.New("BitTorrent v2-only magnet links are not supported yet")

type FileInfo struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
}
