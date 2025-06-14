package settings

import "time"

const (
	hlsPath       = "/tmp/cinemator/hls"
	downloadPath  = "/tmp/cinemator/download"
	viewerTimeout = 7 * 24 * time.Hour
	httpPort      = 8000
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

func (s *Settings) HttpPort() int {
	return httpPort
}
