package torrent

import (
	"cinemator/domain"
	"io"

	"github.com/anacrolix/torrent"
)

type ReadableFile struct {
	file *torrent.File
}

func NewReadableFile(file *torrent.File) domain.ReadableFile {
	return &ReadableFile{
		file: file,
	}
}

func (r ReadableFile) ReadSeekCloser() io.ReadSeekCloser {
	return r.file.NewReader()
}
