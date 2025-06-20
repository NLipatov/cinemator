package domain

import "io"

type ReadableFile interface {
	ReadSeekCloser() io.ReadSeekCloser
}
