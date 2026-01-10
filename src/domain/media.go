package domain

type AudioTrack struct {
	Index    int
	Codec    string
	Language string
	Title    string
}

type SubtitleTrack struct {
	Index    int
	Codec    string
	Language string
	Title    string
}

type MediaInfo struct {
	VideoCodec  string
	NeedFilter  bool
	AudioTracks []AudioTrack
	Subtitles   []SubtitleTrack
}
