package domain

import (
	"errors"
	"time"
)

var (
	ErrBadDownloadID    = errors.New("bad download id")
	ErrDownloadNotFound = errors.New("download not found")
)

type DownloadStatus string

const (
	DownloadStatusReady     DownloadStatus = "ready"
	DownloadStatusStreaming DownloadStatus = "streaming"
	DownloadStatusPaused    DownloadStatus = "paused"
	DownloadStatusExpired   DownloadStatus = "expired"
)

type Download struct {
	ID             string         `json:"id"`
	Magnet         string         `json:"magnet"`
	Title          string         `json:"title"`
	Status         DownloadStatus `json:"status"`
	Size           int64          `json:"size"`
	Files          []FileInfo     `json:"files"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
	LastAccessedAt time.Time      `json:"lastAccessedAt"`
	ExpiresAt      time.Time      `json:"expiresAt"`
}
