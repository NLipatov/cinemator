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

type HlsPhase string

const (
	HlsPhaseProbing   HlsPhase = "probing"
	HlsPhaseWaiting   HlsPhase = "waiting"
	HlsPhasePreparing HlsPhase = "preparing"
	HlsPhaseReady     HlsPhase = "ready"
	HlsPhaseNoPeers   HlsPhase = "no_peers"
	HlsPhaseStalled   HlsPhase = "stalled"
	HlsPhaseError     HlsPhase = "error"
)

type HlsStage string

const (
	HlsStageQueued        HlsStage = "queued"
	HlsStageWaitingSource HlsStage = "waiting_source"
	HlsStageWaitingCPU    HlsStage = "waiting_cpu"
	HlsStagePackaging     HlsStage = "packaging"
	HlsStageSourceBlocked HlsStage = "source_blocked"
	HlsStagePublishing    HlsStage = "publishing"
	HlsStageReady         HlsStage = "ready"
	HlsStageCancelled     HlsStage = "cancelled"
	HlsStageError         HlsStage = "error"
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
	Phase                     HlsPhase  `json:"phase"`
	Stage                     HlsStage  `json:"stage,omitempty"`
	WorkClass                 string    `json:"workClass,omitempty"`
	Generation                string    `json:"generation"`
	Mode                      string    `json:"mode,omitempty"`
	TargetSeconds             float64   `json:"targetSeconds"`
	PresentationOriginSeconds float64   `json:"presentationOriginSeconds"`
	StartedAt                 time.Time `json:"startedAt"`
	LastProgress              time.Time `json:"lastProgress"`
	BytesRead                 int64     `json:"bytesRead"`
	PeerBytes                 int64     `json:"peerBytes,omitempty"`
	SourceRateBitsPerSecond   int64     `json:"sourceRateBitsPerSecond,omitempty"`
	CacheBytes                int64     `json:"cacheBytes,omitempty"`
	PublishedBytes            int64     `json:"publishedBytes,omitempty"`
	RequestedRangeStart       int64     `json:"requestedRangeStart,omitempty"`
	RequestedRangeEnd         int64     `json:"requestedRangeEnd,omitempty"`
	MissingPieces             int       `json:"missingPieces,omitempty"`
	RangePieces               int       `json:"rangePieces,omitempty"`
	ActivePeers               int       `json:"activePeers"`
	TotalPeers                int       `json:"totalPeers"`
	Seekable                  bool      `json:"seekable"`
	Duration                  float64   `json:"duration"`
	Message                   string    `json:"message,omitempty"`
}
