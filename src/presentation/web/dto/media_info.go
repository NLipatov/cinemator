package dto

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
	AudioTracks []AudioTrack    `json:"audioTracks"`
	Subtitles   []SubtitleTrack `json:"subtitles"`
}
