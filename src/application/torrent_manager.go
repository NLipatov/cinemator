package application

import (
	"context"
	"io"
	"time"

	"cinemator/domain"
)

type ReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

type HlsAsset struct {
	ReadSeekCloser
	ModTime time.Time
}

type TorrentManager interface {
	GetTorrentFiles(ctx context.Context, magnet string) ([]domain.FileInfo, error)
	GetMediaInfo(ctx context.Context, magnet string, fileIndex int) (domain.MediaInfo, error)
	PrepareHlsStream(ctx context.Context, magnet string, fileIndex, audioTrack, subtitleTrack int, startSeconds float64, forceTranscode bool) (playlistPath string, err error)
	OpenHlsAsset(ctx context.Context, streamDir, assetName, version string) (HlsAsset, error)
	GetHlsStatus(ctx context.Context, streamDir string, targetSeconds float64) (domain.HlsStatus, error)
	TouchStream(ctx context.Context, dirName string)
	ListDownloads(ctx context.Context) ([]domain.Download, error)
	ExtendDownload(ctx context.Context, id string, extension time.Duration) (domain.Download, error)
	DeleteDownload(ctx context.Context, id string) error
	SubscribeDownloadEvents(ctx context.Context) <-chan struct{}
	CleanupStreams()
	Close() error
}
