package torrent

import (
	"errors"
	"time"
)

var (
	ErrBadDownloadID    = errors.New("bad download id")
	ErrDownloadNotFound = errors.New("download not found")
	ErrDownloadNotReady = errors.New("download is not ready")
)

type DownloadStatus string

const (
	DownloadStatusAwaitingSelection DownloadStatus = "awaiting_selection"
	DownloadStatusPreparing         DownloadStatus = "preparing"
	DownloadStatusReady             DownloadStatus = "ready"
	DownloadStatusFailed            DownloadStatus = "failed"
	DownloadStatusExpired           DownloadStatus = "expired"
)

type Download struct {
	ID                string         `json:"id"`
	Magnet            string         `json:"magnet"`
	Title             string         `json:"title"`
	Status            DownloadStatus `json:"status"`
	Size              int64          `json:"size"`
	DiskSize          int64          `json:"diskSize,omitempty"`
	Files             []FileInfo     `json:"files"`
	SelectedFileIndex *int           `json:"selectedFileIndex,omitempty"`
	ReadyAt           time.Time      `json:"readyAt,omitempty"`
	PreparationErr    string         `json:"preparationError,omitempty"`
	CreatedAt         time.Time      `json:"createdAt"`
	UpdatedAt         time.Time      `json:"updatedAt"`
	LastAccessedAt    time.Time      `json:"lastAccessedAt"`
	ExpiresAt         time.Time      `json:"expiresAt"`
}
