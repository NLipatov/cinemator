package settings

import (
	"os"
	"strconv"
	"time"
)

const (
	defaultHlsPath              = "/var/tmp/cinemator/hls"
	defaultDownloadPath         = "/var/tmp/cinemator/download"
	defaultViewerTimeout        = 2 * time.Hour
	defaultMaxCacheBytes        = 2 << 30  // 2 GiB cap for generated HLS assets
	defaultMaxTorrentCacheBytes = 12 << 30 // 12 GiB cap for verified torrent pieces
	defaultTorrentReadahead     = 512 << 20
	defaultHlsSegmentDuration   = 6 * time.Second
	defaultHlsWindowSegments    = 5
	defaultMaxTranscodes        = 1
	defaultHTTPPort             = 8000
	defaultTorrentPort          = 42069
)

type Settings struct {
	hlsPath              string
	downloadPath         string
	viewerTimeout        time.Duration
	maxCacheBytes        int64
	maxTorrentCacheBytes int64
	torrentReadahead     int64
	hlsSegmentDuration   time.Duration
	hlsWindowSegments    int
	maxTranscodes        int
	httpPort             int
	torrentPort          int
	passwordHash         string
	sessionSecret        string
}

func NewSettings() Settings {
	maxTorrentCacheBytes := int64Env("CINEMATOR_MAX_TORRENT_CACHE_BYTES", defaultMaxTorrentCacheBytes)
	torrentReadahead := int64Env("CINEMATOR_TORRENT_READAHEAD_BYTES", defaultTorrentReadahead)
	if torrentReadahead <= 0 {
		torrentReadahead = defaultTorrentReadahead
	}
	if maxTorrentCacheBytes > 0 && torrentReadahead > maxTorrentCacheBytes/4 {
		torrentReadahead = max(1, maxTorrentCacheBytes/4)
	}
	return Settings{
		hlsPath:              stringEnv("CINEMATOR_HLS_PATH", defaultHlsPath),
		downloadPath:         stringEnv("CINEMATOR_DOWNLOAD_PATH", defaultDownloadPath),
		viewerTimeout:        defaultViewerTimeout,
		maxCacheBytes:        int64Env("CINEMATOR_MAX_CACHE_BYTES", defaultMaxCacheBytes),
		maxTorrentCacheBytes: maxTorrentCacheBytes,
		torrentReadahead:     torrentReadahead,
		hlsSegmentDuration:   time.Duration(intEnv("CINEMATOR_HLS_SEGMENT_SECONDS", int(defaultHlsSegmentDuration/time.Second))) * time.Second,
		hlsWindowSegments:    intEnv("CINEMATOR_HLS_WINDOW_SEGMENTS", defaultHlsWindowSegments),
		maxTranscodes:        intEnv("CINEMATOR_MAX_TRANSCODES", defaultMaxTranscodes),
		httpPort:             intEnv("CINEMATOR_HTTP_PORT", defaultHTTPPort),
		torrentPort:          intEnv("CINEMATOR_TORRENT_PORT", defaultTorrentPort),
		passwordHash:         stringEnv("CINEMATOR_PASSWORD_HASH", ""),
		sessionSecret:        stringEnv("CINEMATOR_SESSION_SECRET", ""),
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

func (s Settings) MaxTorrentCacheBytes() int64 {
	return s.maxTorrentCacheBytes
}

func (s Settings) TorrentReadaheadBytes() int64 {
	return s.torrentReadahead
}

func (s Settings) HlsSegmentDuration() time.Duration {
	if s.hlsSegmentDuration <= 0 {
		return defaultHlsSegmentDuration
	}
	return s.hlsSegmentDuration
}

func (s Settings) HlsWindowSegments() int {
	if s.hlsWindowSegments <= 0 {
		return defaultHlsWindowSegments
	}
	return s.hlsWindowSegments
}

func (s Settings) MaxTranscodes() int {
	if s.maxTranscodes <= 0 {
		return defaultMaxTranscodes
	}
	return s.maxTranscodes
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
