package mappers

import (
	"cinemator/application"
	"cinemator/domain"
	"cinemator/presentation/web/dto"
)

type DirInfoMapper struct {
}

func NewDirInfoMapper() application.Mapper[domain.DirInfo, dto.DirInfo] {
	return DirInfoMapper{}
}

func (d DirInfoMapper) Map(source domain.DirInfo) dto.DirInfo {
	return dto.DirInfo{
		Name:   source.Name,
		SizeGB: source.SizeGB,
	}
}

func (d DirInfoMapper) MapArray(sources []domain.DirInfo) []dto.DirInfo {
	result := make([]dto.DirInfo, len(sources))
	for i := 0; i < len(sources); i++ {
		result[i] = d.Map(sources[i])
	}
	return result
}
