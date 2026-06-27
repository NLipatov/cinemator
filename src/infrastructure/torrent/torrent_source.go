package torrent

import (
	"context"
	"fmt"
	"time"

	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"

	"github.com/anacrolix/torrent"
)

const (
	initialProbeBytes = 1 << 20
	maxProbeBytes     = 16 << 20
	probeTailBytes    = 1 << 20
)

type torrentSource struct {
	file      *torrent.File
	registry  *rangeServer
	token     string
	url       string
	readahead int64
}

type filePieceWindow struct {
	index int
	state torrent.FilePieceState
}

func newTorrentSource(file *torrent.File, registry *rangeServer) (*torrentSource, error) {
	readahead := targetReadaheadBytes(file)
	token, sourceURL, err := registry.register(file, readahead)
	if err != nil {
		return nil, err
	}
	return &torrentSource{
		file:      file,
		registry:  registry,
		token:     token,
		url:       sourceURL,
		readahead: readahead,
	}, nil
}

func (s *torrentSource) URL() string {
	return s.url
}

func (s *torrentSource) Close() {
	if s == nil || s.registry == nil {
		return
	}
	s.registry.unregister(s.token)
	s.registry = nil
}

func (s *torrentSource) Probe(ctx context.Context) (domain.MediaInfo, error) {
	s.PrefetchRange(0, initialProbeBytes)
	if err := s.WaitRange(ctx, 0, initialProbeBytes); err != nil {
		return domain.MediaInfo{}, err
	}

	reader := s.file.NewReader()
	reader.SetContext(ctx)
	reader.SetReadahead(maxProbeBytes)
	defer reader.Close()
	if info, err := (ffmpeg.SampleAnalyzer{}).Analyze(reader); err == nil {
		return info, nil
	}

	if tailOffset := s.file.Length() - probeTailBytes; tailOffset > 0 {
		s.PrefetchRange(tailOffset, probeTailBytes)
	}
	return (ffmpeg.SampleAnalyzer{}).AnalyzeURL(ctx, s.url)
}

func (s *torrentSource) PrefetchRange(offset, length int64) {
	windows := s.rangeWindows(offset, length)
	if len(windows) == 0 {
		return
	}
	s.file.Torrent().DownloadPieces(windows[0].index, windows[len(windows)-1].index+1)
}

func (s *torrentSource) WaitRange(ctx context.Context, offset, length int64) error {
	offset, length = clampRange(offset, length, s.file.Length())
	if length <= 0 {
		return nil
	}

	s.PrefetchRange(offset, length)
	if err := s.verifyRangeOnce(ctx, offset, length); err != nil {
		return err
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		complete, err := s.rangeComplete(offset, length)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *torrentSource) rangeComplete(offset, length int64) (bool, error) {
	windows := s.rangeWindows(offset, length)
	if len(windows) == 0 {
		return true, nil
	}
	for _, w := range windows {
		if w.state.Err != nil {
			return false, w.state.Err
		}
		if !w.state.Ok || !w.state.Complete {
			return false, nil
		}
	}
	return true, nil
}

func (s *torrentSource) verifyRangeOnce(ctx context.Context, offset, length int64) error {
	windows := s.rangeWindows(offset, length)
	for _, w := range windows {
		if w.state.Ok && w.state.Complete {
			continue
		}
		piece := s.file.Torrent().Piece(w.index)
		if err := piece.VerifyDataContext(ctx); err != nil {
			return err
		}
		if state := piece.State(); state.Err != nil {
			return state.Err
		}
	}
	return nil
}

func (s *torrentSource) rangeWindows(offset, length int64) []filePieceWindow {
	offset, length = clampRange(offset, length, s.file.Length())
	if length <= 0 {
		return nil
	}

	rangeStart := offset
	rangeEnd := offset + length
	pieceIndex := s.file.BeginPieceIndex()
	pieceStart := int64(0)
	var windows []filePieceWindow
	for _, state := range s.file.State() {
		pieceEnd := pieceStart + state.Bytes
		if pieceEnd > rangeStart && pieceStart < rangeEnd {
			windows = append(windows, filePieceWindow{
				index: pieceIndex,
				state: state,
			})
		}
		pieceStart = pieceEnd
		pieceIndex++
		if pieceStart >= rangeEnd {
			break
		}
	}
	return windows
}

func clampRange(offset, length, fileLength int64) (int64, int64) {
	if offset < 0 {
		length += offset
		offset = 0
	}
	if length <= 0 || fileLength <= 0 || offset >= fileLength {
		return offset, 0
	}
	if length > fileLength-offset {
		length = fileLength - offset
	}
	return offset, length
}

func targetReadaheadBytes(f *torrent.File) int64 {
	const (
		targetBufferBytes = int64(1 << 30)
		minAheadBytes     = int64(128 << 20)
	)
	ahead := targetBufferBytes
	if ahead < minAheadBytes {
		ahead = minAheadBytes
	}
	if l := f.Length(); l > 0 && l < ahead {
		ahead = l
	}
	return ahead
}

func addMagnet(client *torrent.Client, magnet string) (*torrent.Torrent, error) {
	spec, err := torrent.TorrentSpecFromMagnetUri(magnet)
	if err != nil {
		return nil, err
	}
	spec.IgnoreUnverifiedPieceCompletion = true
	t, _, err := client.AddTorrentSpec(spec)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("torrent not created")
	}
	return t, nil
}
