package torrent

import (
	"context"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

type streamKey struct {
	InfoHash string
	Index    int
}

type streamInfo struct {
	ready    chan struct{}
	cancel   context.CancelFunc
	torrent  *torrent.Torrent
	file     *torrent.File
	viewers  int
	lastView time.Time
	mtx      sync.Mutex
}
