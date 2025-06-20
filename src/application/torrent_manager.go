package application

import (
	"cinemator/domain"
	"context"

	"github.com/anacrolix/torrent"
)

type TorrentManager interface {
	GetTorrentFiles(ctx context.Context, magnet string) ([]domain.FileInfo, error)
	PrepareHlsStream(ctx context.Context, magnet string, fileIndex int) (*torrent.File, string, string, context.CancelFunc, error)
}
