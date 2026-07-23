package settings

import (
	"os"
	"strconv"
	"time"
)

const (
	defaultHlsPath            = "/var/tmp/cinemator/hls"
	defaultDownloadPath       = "/var/tmp/cinemator/download"
	defaultViewerTimeout      = 30 * time.Minute
	defaultMaxCacheBytes      = 12 << 30 // shared cap for generated HLS and verified torrent pieces
	legacyHlsCacheBytes       = 2 << 30
	legacyTorrentCacheBytes   = 12 << 30
	defaultMinFreeBytes       = 2 << 30 // emergency floor shared by all Cinemator cache writers
	defaultMinFreeInodes      = 4096
	defaultTorrentReadahead   = 64 << 20
	defaultHlsSegmentDuration = 2 * time.Second
	defaultHlsWindowSegments  = 15
	defaultMaxTranscodes      = 1
	defaultMaxPackagers       = 1
	defaultMaxQueuedJobs      = 4
	defaultMaxJobsPerStream   = 3
	defaultMaxActiveStreams   = 16
	defaultHTTPPort           = 8000
	defaultTorrentPort        = 42069
)

type Settings struct {
	hlsPath            string
	downloadPath       string
	viewerTimeout      time.Duration
	maxCacheBytes      int64
	minFreeBytes       int64
	minFreeInodes      uint64
	torrentReadahead   int64
	hlsSegmentDuration time.Duration
	hlsWindowSegments  int
	maxTranscodes      int
	maxPackagers       int
	maxQueuedJobs      int
	maxJobsPerStream   int
	maxActiveStreams   int
	httpPort           int
	torrentPort        int
	passwordHash       string
	sessionSecret      string
}

func NewSettings() Settings {
	maxCacheBytes := totalCacheBytes()
	torrentReadahead := int64Env("CINEMATOR_TORRENT_READAHEAD_BYTES", defaultTorrentReadahead)
	if torrentReadahead <= 0 {
		torrentReadahead = defaultTorrentReadahead
	}
	if maxCacheBytes > 0 && torrentReadahead > maxCacheBytes/4 {
		torrentReadahead = max(1, maxCacheBytes/4)
	}
	return Settings{
		hlsPath:            stringEnv("CINEMATOR_HLS_PATH", defaultHlsPath),
		downloadPath:       stringEnv("CINEMATOR_DOWNLOAD_PATH", defaultDownloadPath),
		viewerTimeout:      defaultViewerTimeout,
		maxCacheBytes:      maxCacheBytes,
		minFreeBytes:       int64Env("CINEMATOR_MIN_FREE_BYTES", defaultMinFreeBytes),
		minFreeInodes:      uint64(max(0, intEnv("CINEMATOR_MIN_FREE_INODES", defaultMinFreeInodes))),
		torrentReadahead:   torrentReadahead,
		hlsSegmentDuration: time.Duration(intEnv("CINEMATOR_HLS_SEGMENT_SECONDS", int(defaultHlsSegmentDuration/time.Second))) * time.Second,
		hlsWindowSegments:  intEnv("CINEMATOR_HLS_WINDOW_SEGMENTS", defaultHlsWindowSegments),
		maxTranscodes:      intEnv("CINEMATOR_MAX_TRANSCODES", defaultMaxTranscodes),
		maxPackagers:       intEnv("CINEMATOR_MAX_PACKAGERS", defaultMaxPackagers),
		maxQueuedJobs:      intEnv("CINEMATOR_MAX_QUEUED_JOBS", defaultMaxQueuedJobs),
		maxJobsPerStream:   intEnv("CINEMATOR_MAX_JOBS_PER_STREAM", defaultMaxJobsPerStream),
		maxActiveStreams:   intEnv("CINEMATOR_MAX_ACTIVE_STREAMS", defaultMaxActiveStreams),
		httpPort:           intEnv("CINEMATOR_HTTP_PORT", defaultHTTPPort),
		torrentPort:        intEnv("CINEMATOR_TORRENT_PORT", defaultTorrentPort),
		passwordHash:       stringEnv("CINEMATOR_PASSWORD_HASH", ""),
		sessionSecret:      stringEnv("CINEMATOR_SESSION_SECRET", ""),
	}
}

func totalCacheBytes() int64 {
	if _, ok := os.LookupEnv("CINEMATOR_TOTAL_CACHE_BYTES"); ok {
		return int64Env("CINEMATOR_TOTAL_CACHE_BYTES", defaultMaxCacheBytes)
	}
	_, oldHls := os.LookupEnv("CINEMATOR_MAX_CACHE_BYTES")
	_, oldTorrent := os.LookupEnv("CINEMATOR_MAX_TORRENT_CACHE_BYTES")
	if oldHls || oldTorrent {
		return int64Env("CINEMATOR_MAX_CACHE_BYTES", legacyHlsCacheBytes) +
			int64Env("CINEMATOR_MAX_TORRENT_CACHE_BYTES", legacyTorrentCacheBytes)
	}
	return defaultMaxCacheBytes
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

func (s Settings) MinFreeBytes() int64 {
	return max(0, s.minFreeBytes)
}

func (s Settings) MinFreeInodes() uint64 {
	return s.minFreeInodes
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

func (s Settings) MaxPackagers() int {
	if s.maxPackagers <= 0 {
		return defaultMaxPackagers
	}
	return s.maxPackagers
}

func (s Settings) MaxQueuedJobs() int {
	if s.maxQueuedJobs <= 0 {
		return defaultMaxQueuedJobs
	}
	return s.maxQueuedJobs
}

func (s Settings) MaxJobsPerStream() int {
	if s.maxJobsPerStream <= 0 {
		return defaultMaxJobsPerStream
	}
	return s.maxJobsPerStream
}

func (s Settings) MaxActiveStreams() int {
	if s.maxActiveStreams <= 0 {
		return defaultMaxActiveStreams
	}
	return s.maxActiveStreams
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
