package torrent

import (
	"cinemator/infrastructure/ffmpeg"
	"context"
	"fmt"
	"path/filepath"
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
	ready     chan struct{}
	cancel    context.CancelFunc
	torrent   *torrent.Torrent
	file      *torrent.File
	lastView  time.Time
	mtx       sync.Mutex
	selection ffmpeg.StreamSelection
	paths     streamPaths
	source    *torrentSource
	runID     uint64
	readyErr  error
	readySent bool
	paused    bool
	running   bool
	completed bool
}

type streamPaths struct {
	outDir           string
	videoPlaylist    string
	subtitlePlaylist string
	masterPlaylist   string
}

func (s *streamInfo) beginRun() uint64 {
	s.mtx.Lock()
	s.runID++
	s.ready = make(chan struct{})
	s.readyErr = nil
	s.readySent = false
	s.completed = false
	runID := s.runID
	s.mtx.Unlock()
	return runID
}

func (s *streamInfo) signalReady(runID uint64, err error) bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if runID != s.runID || s.readySent {
		return false
	}
	s.readyErr = err
	s.readySent = true
	if s.ready != nil {
		close(s.ready)
	}
	return true
}

func (s *streamInfo) isCurrentRun(runID uint64) bool {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return runID == s.runID
}

func (s *streamInfo) waitReady(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.mtx.Lock()
	ready := s.ready
	readyErr := s.readyErr
	readySent := s.readySent
	s.mtx.Unlock()

	if ready == nil || readySent {
		return readyErr
	}

	select {
	case <-ready:
		return s.readyError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *streamInfo) readyError() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.readyErr
}

func (k streamKey) dirName() string {
	return fmt.Sprintf("%s_%d_a%d_s%d", k.InfoHash, k.Index, k.Audio, k.Subtitle)
}

func (k streamKey) paths(root string) streamPaths {
	outDir := filepath.Join(root, k.dirName())
	return streamPaths{
		outDir:           outDir,
		videoPlaylist:    filepath.Join(outDir, "index.m3u8"),
		subtitlePlaylist: filepath.Join(outDir, "subs.m3u8"),
		masterPlaylist:   filepath.Join(outDir, "master.m3u8"),
	}
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
