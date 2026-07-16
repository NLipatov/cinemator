package settings

import (
	"os"
	"strconv"
	"time"
)

const (
	defaultHlsPath       = "/var/tmp/cinemator/hls"
	defaultDownloadPath  = "/var/tmp/cinemator/download"
	defaultViewerTimeout = 2 * time.Hour
	defaultMaxCacheBytes = 2 << 30 // 2 GiB cap for HLS cache
	defaultHTTPPort      = 8000
	defaultTorrentPort   = 42069
)

type Settings struct {
	hlsPath       string
	downloadPath  string
	viewerTimeout time.Duration
	maxCacheBytes int64
	httpPort      int
	torrentPort   int
	passwordHash  string
	sessionSecret string
}

func NewSettings() Settings {
	return Settings{
		hlsPath:       stringEnv("CINEMATOR_HLS_PATH", defaultHlsPath),
		downloadPath:  stringEnv("CINEMATOR_DOWNLOAD_PATH", defaultDownloadPath),
		viewerTimeout: defaultViewerTimeout,
		maxCacheBytes: int64Env("CINEMATOR_MAX_CACHE_BYTES", defaultMaxCacheBytes),
		httpPort:      intEnv("CINEMATOR_HTTP_PORT", defaultHTTPPort),
		torrentPort:   intEnv("CINEMATOR_TORRENT_PORT", defaultTorrentPort),
		passwordHash:  stringEnv("CINEMATOR_PASSWORD_HASH", ""),
		sessionSecret: stringEnv("CINEMATOR_SESSION_SECRET", ""),
	}
}

func (s Settings) HlsPath() string {
	return s.hlsPath
}

func (s Settings) DownloadPath() string {
	return s.downloadPath
}

func (s Settings) ViewerTimeout() time.Duration {
	return s.viewerTimeout
}

func (s Settings) MaxCacheBytes() int64 {
	return s.maxCacheBytes
}

func (s Settings) HttpPort() int {
	return s.httpPort
}

func (s Settings) TorrentPort() int {
	return s.torrentPort
}

func (s Settings) PasswordHash() string {
	return s.passwordHash
}

func (s Settings) SessionSecret() string {
	return s.sessionSecret
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

func int64Env(key string, fallback int64) int64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
