package application

import (
	"cinemator/domain"
	"context"
)

type TorrentManager interface {
	Files(ctx context.Context, magnet string) ([]domain.FileInfo, error)
	StartStream(ctx context.Context, magnet string, fileIndex int) (playlistPath, hlsDir string, cancel context.CancelFunc, err error)
	TouchStream(magnet string, fileIndex int)
	CleanupStreams()
}
