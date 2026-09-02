package web

import (
	"context"
	"time"

	"cinemator/media"
	"cinemator/torrent"
)

type torrentManager interface {
	GetTorrentFiles(ctx context.Context, magnet string) ([]torrent.FileInfo, error)
	GetMediaInfo(ctx context.Context, magnet string, fileIndex int) (media.MediaInfo, error)
	StartHLSPreparation(ctx context.Context, magnet string, fileIndex int) error
	PrepareHlsStream(ctx context.Context, magnet string, fileIndex, audioTrack, subtitleTrack int) (playlistPath string, err error)
	TouchStream(ctx context.Context, dirName string)
	ListDownloads(ctx context.Context) ([]torrent.Download, error)
	ExtendDownload(ctx context.Context, id string, extension time.Duration) (torrent.Download, error)
	DeleteDownload(ctx context.Context, id string) error
	SubscribeDownloadEvents(ctx context.Context) <-chan struct{}
}
