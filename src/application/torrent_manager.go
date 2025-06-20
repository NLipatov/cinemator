package application

import (
	"cinemator/domain"
	"context"

	"github.com/anacrolix/torrent"
)

type TorrentManager interface {
	ListFiles(ctx context.Context, magnet string) ([]domain.FileInfo, error)
	InfoHash(ctx context.Context, magnet string) (string, error)
	DownloadFile(ctx context.Context, magnet string, fileIndex int) (*torrent.File, error)
}
