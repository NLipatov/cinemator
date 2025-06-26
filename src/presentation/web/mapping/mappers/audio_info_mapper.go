package mappers

import (
	"cinemator/application"
	"cinemator/domain"
	"cinemator/presentation/web/dto"
)

type AudioInfoMapper struct {
}

func NewAudioInfoMapper() application.Mapper[domain.AudioInfo, dto.AudioInfo] {
	return &AudioInfoMapper{}
}

func (a AudioInfoMapper) Map(s domain.AudioInfo) dto.AudioInfo {
	return dto.AudioInfo{
		Title:    s.Tag.Title,
		Language: s.Tag.Language,
	}
}

func (a AudioInfoMapper) MapArray(s []domain.AudioInfo) []dto.AudioInfo {
	result := make([]dto.AudioInfo, len(s))
	for i := 0; i < len(s); i++ {
		result[i] = a.Map(s[i])
	}
	return result
}
