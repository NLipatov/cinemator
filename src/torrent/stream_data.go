package torrent

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"cinemator/media"

	"github.com/anacrolix/torrent"
)

type streamKey struct {
	InfoHash string
	Index    int
	Audio    int
	Subtitle int
}

type streamInfo struct {
	playable       chan struct{}
	cancel         context.CancelFunc
	torrent        *torrent.Torrent
	file           *torrent.File
	lastView       time.Time
	mtx            sync.Mutex
	mediaInfo      media.MediaInfo
	bitmapSubtitle int
	paths          streamPaths
	source         *torrentSource
	runDone        chan struct{}
	playableErr    error
	playableSent   bool
	completed      bool
}

type streamPaths struct {
	outDir         string
	videoPlaylist  string
	masterPlaylist string
	readyMarker    string
}

func (s *streamInfo) signalPlayable(err error) bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.playableSent {
		return false
	}
	s.playableErr = err
	s.playableSent = true
	if s.playable != nil {
		close(s.playable)
	}
	return true
}

func (s *streamInfo) waitPlayable(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.mtx.Lock()
	playable := s.playable
	playableErr := s.playableErr
	playableSent := s.playableSent
	s.mtx.Unlock()

	if playable == nil || playableSent {
		return playableErr
	}

	select {
	case <-playable:
		return s.playableError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *streamInfo) playableError() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.playableErr
}

func (k streamKey) dirName() string {
	return fmt.Sprintf("%s_%d_a%d_s%d", k.InfoHash, k.Index, k.Audio, k.Subtitle)
}

func (k streamKey) paths(root string) streamPaths {
	outDir := filepath.Join(root, k.dirName())
	return streamPaths{
		outDir:         outDir,
		videoPlaylist:  filepath.Join(outDir, "index.m3u8"),
		masterPlaylist: filepath.Join(outDir, "master.m3u8"),
		readyMarker:    filepath.Join(outDir, ".ready"),
	}
}

func (p streamPaths) selectionMaster(audioTrack, subtitleTrack int) string {
	return filepath.Join(p.outDir, fmt.Sprintf("master_a%d_s%d.m3u8", audioTrack, subtitleTrack))
}

func parseStreamDir(name string) (streamKey, error) {
	parts := strings.Split(name, "_")
	if len(parts) != 4 {
		return streamKey{}, fmt.Errorf("bad stream dir")
	}
	if !strings.HasPrefix(parts[2], "a") || !strings.HasPrefix(parts[3], "s") {
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
