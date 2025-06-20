package application

import (
	"cinemator/domain"
	"context"
)

type Downloader interface {
	ListFiles(ctx context.Context, magnet string) ([]domain.FileInfo, error)
	InfoHash(ctx context.Context, magnet string) (string, error)
	DownloadFile(ctx context.Context, magnet string, fileIndex int) (domain.ReadableFile, error)
}
