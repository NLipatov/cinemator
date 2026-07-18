package domain

import (
	"errors"
	"time"
)

var (
	ErrBadHlsRequest             = errors.New("bad HLS request")
	ErrHlsStreamNotFound         = errors.New("HLS stream not found")
	ErrHlsAssetUnsupported       = errors.New("unsupported HLS asset")
	ErrHlsTemporarilyUnavailable = errors.New("HLS stream temporarily unavailable")
	ErrHlsPlaylistChanged        = errors.New("HLS playlist changed")
)

type AudioTrack struct {
	Index      int    `json:"index"`
	Codec      string `json:"codec"`
	Profile    string `json:"profile,omitempty"`
	Channels   int    `json:"channels,omitempty"`
	SampleRate int    `json:"sampleRate,omitempty"`
	Language   string `json:"language"`
	Title      string `json:"title"`
}

type SubtitleTrack struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Language string `json:"language"`
	Title    string `json:"title"`
}

type MediaInfo struct {
	VideoCodec       string          `json:"videoCodec,omitempty"`
	VideoCodecString string          `json:"videoCodecString,omitempty"`
	VideoProfile     string          `json:"videoProfile,omitempty"`
	VideoLevel       int             `json:"videoLevel,omitempty"`
	VideoTrackIndex  int             `json:"-"`
	Width            int             `json:"width,omitempty"`
	Height           int             `json:"height,omitempty"`
	FrameRate        float64         `json:"frameRate,omitempty"`
	PixelFormat      string          `json:"pixelFormat,omitempty"`
	BitDepth         int             `json:"bitDepth,omitempty"`
	ColorPrimaries   string          `json:"colorPrimaries,omitempty"`
	ColorTransfer    string          `json:"colorTransfer,omitempty"`
	ColorSpace       string          `json:"colorSpace,omitempty"`
	Rotated          bool            `json:"rotated,omitempty"`
	HDR              bool            `json:"hdr,omitempty"`
	HDRFormat        string          `json:"hdrFormat,omitempty"`
	DolbyVision      bool            `json:"dolbyVision,omitempty"`
	NeedFilter       bool            `json:"-"`
	Deinterlace      bool            `json:"interlaced,omitempty"`
	Duration         float64         `json:"duration"`
	Seekable         bool            `json:"seekable"`
	Bitrate          int64           `json:"bitrate,omitempty"`
	AudioTracks      []AudioTrack    `json:"audioTracks"`
	Subtitles        []SubtitleTrack `json:"subtitles"`
	Warnings         []string        `json:"warnings,omitempty"`
}

type HlsStatus struct {
	Phase         string    `json:"phase"`
	Mode          string    `json:"mode,omitempty"`
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
