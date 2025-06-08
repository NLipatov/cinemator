package mappers

import (
	"cinemator/application"
	"cinemator/domain"
	"cinemator/presentation/dto"
)

type FileInfoMapper struct {
}

func NewFileInfoMapper() application.Mapper[domain.FileInfo, dto.FileInfo] {
	return FileInfoMapper{}
}

func (f FileInfoMapper) Map(s domain.FileInfo) dto.FileInfo {
	return dto.FileInfo{
		Index: s.Index,
		Name:  s.Name,
		Size:  s.Size,
	}
}

func (f FileInfoMapper) MapArray(s []domain.FileInfo) []dto.FileInfo {
	result := make([]dto.FileInfo, len(s))
	for i := 0; i < len(s); i++ {
		result[i] = f.Map(s[i])
	}
	return result
}
