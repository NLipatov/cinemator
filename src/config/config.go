package config

import (
	"os"
	"strconv"
	"time"
)

const (
	defaultHlsPath       = "/var/tmp/cinemator/hls"
	defaultDownloadPath  = "/var/tmp/cinemator/download"
	defaultViewerTimeout = 2 * time.Hour
	defaultHTTPPort      = 8000
	defaultTorrentPort   = 42069
)

type Config struct {
	HLSPath       string
	DownloadPath  string
	ViewerTimeout time.Duration
	HTTPPort      int
	TorrentPort   int
	PasswordHash  string
	SessionSecret string
}

func Load() Config {
	return Config{
		HLSPath:       stringEnv("CINEMATOR_HLS_PATH", defaultHlsPath),
		DownloadPath:  stringEnv("CINEMATOR_DOWNLOAD_PATH", defaultDownloadPath),
		ViewerTimeout: defaultViewerTimeout,
		HTTPPort:      intEnv("CINEMATOR_HTTP_PORT", defaultHTTPPort),
		TorrentPort:   intEnv("CINEMATOR_TORRENT_PORT", defaultTorrentPort),
		PasswordHash:  stringEnv("CINEMATOR_PASSWORD_HASH", ""),
		SessionSecret: stringEnv("CINEMATOR_SESSION_SECRET", ""),
	}
}

func stringEnv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func intEnv(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
