package settings

import "time"

const (
	hlsPath         = "/var/tmp/cinemator/hls"
	downloadPath    = "/var/tmp/cinemator/download"
	viewerTimeout   = 2 * time.Hour
	maxCacheBytes   = 2 << 30 // 2 GiB cap for HLS cache
	httpPort        = 8000
	minProbeSizeMiB = 1
)

type Settings struct {
}

func NewSettings() Settings {
	return Settings{}
}

func (s *Settings) HlsPath() string {
	return hlsPath
}

func (s *Settings) DownloadPath() string {
	return downloadPath
}

func (s *Settings) ViewerTimeout() time.Duration {
	return viewerTimeout
}

func (s *Settings) MaxCacheBytes() int64 {
	return maxCacheBytes
}

func (s *Settings) HttpPort() int {
	return httpPort
}

func (s *Settings) MinProbeSizeMiB() int {
	return minProbeSizeMiB
}
