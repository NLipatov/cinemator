package mappers

import (
	"cinemator/domain"
	"cinemator/presentation/web/dto"
)

type MediaInfoMapper struct{}

func NewMediaInfoMapper() *MediaInfoMapper {
	return &MediaInfoMapper{}
}

func (m *MediaInfoMapper) Map(src domain.MediaInfo) dto.MediaInfo {
	audioTracks := make([]dto.AudioTrack, len(src.AudioTracks))
	for i, a := range src.AudioTracks {
		audioTracks[i] = dto.AudioTrack{
			Index:    a.Index,
			Codec:    a.Codec,
			Language: a.Language,
			Title:    a.Title,
		}
	}

	subtitles := make([]dto.SubtitleTrack, len(src.Subtitles))
	for i, s := range src.Subtitles {
		subtitles[i] = dto.SubtitleTrack{
			Index:    s.Index,
			Codec:    s.Codec,
			Language: s.Language,
			Title:    s.Title,
		}
	}

	return dto.MediaInfo{
		AudioTracks: audioTracks,
		Subtitles:   subtitles,
	}
}
