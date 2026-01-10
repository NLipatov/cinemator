package torrent

import (
	"cinemator/infrastructure/ffmpeg"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

type streamKey struct {
	InfoHash string
	Index    int
	Audio    int
	Subtitle int
}

type streamInfo struct {
	ready            chan struct{}
	cancel           context.CancelFunc
	torrent          *torrent.Torrent
	file             *torrent.File
	lastView         time.Time
	mtx              sync.Mutex
	selection        ffmpeg.StreamSelection
	outDir           string
	videoPlaylist    string
	subtitlePlaylist string
	masterPlaylist   string
	paused           bool
	running          bool
}

func (k streamKey) dirName() string {
	return fmt.Sprintf("%s_%d_a%d_s%d", k.InfoHash, k.Index, k.Audio, k.Subtitle)
}

func parseStreamDir(name string) (streamKey, error) {
	parts := strings.Split(name, "_")
	if len(parts) != 4 {
		return streamKey{}, fmt.Errorf("bad stream dir")
	}
	audio := strings.TrimPrefix(parts[2], "a")
	subtitle := strings.TrimPrefix(parts[3], "s")
	idx, err := strconv.Atoi(parts[1])
	if err != nil {
		return streamKey{}, err
	}
	audioIdx, err := strconv.Atoi(audio)
	if err != nil {
		return streamKey{}, err
	}
	subIdx, err := strconv.Atoi(subtitle)
	if err != nil {
		return streamKey{}, err
	}
	return streamKey{
		InfoHash: parts[0],
		Index:    idx,
		Audio:    audioIdx,
		Subtitle: subIdx,
	}, nil
}
