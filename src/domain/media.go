package domain

import "time"

type AudioTrack struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Language string `json:"language"`
	Title    string `json:"title"`
}

type SubtitleTrack struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Language string `json:"language"`
	Title    string `json:"title"`
}

type MediaInfo struct {
	VideoCodec  string          `json:"-"`
	Width       int             `json:"-"`
	Height      int             `json:"-"`
	HDR         bool            `json:"-"`
	NeedFilter  bool            `json:"-"`
	Duration    float64         `json:"duration"`
	Seekable    bool            `json:"seekable"`
	Bitrate     int64           `json:"-"`
	AudioTracks []AudioTrack    `json:"audioTracks"`
	Subtitles   []SubtitleTrack `json:"subtitles"`
}

type HlsStatus struct {
	Phase         string    `json:"phase"`
	TargetSeconds float64   `json:"targetSeconds"`
	StartedAt     time.Time `json:"startedAt"`
	LastProgress  time.Time `json:"lastProgress"`
	BytesRead     int64     `json:"bytesRead"`
	ActivePeers   int       `json:"activePeers"`
	TotalPeers    int       `json:"totalPeers"`
	Seekable      bool      `json:"seekable"`
	Duration      float64   `json:"duration"`
	Message       string    `json:"message,omitempty"`
}
