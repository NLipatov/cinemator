package application

import (
	"cinemator/domain"
	"context"
)

type TorrentManager interface {
	GetTorrentFiles(ctx context.Context, magnet string) ([]domain.FileInfo, error)
	GetMediaInfo(ctx context.Context, magnet string, fileIndex int) (domain.MediaInfo, error)
	PrepareHlsStream(ctx context.Context, magnet string, fileIndex, audioTrack, subtitleTrack int) (playlistPath, hlsDir string, cancel context.CancelFunc, err error)
	TouchStream(ctx context.Context, dirName string)
	CleanupStreams()
}
