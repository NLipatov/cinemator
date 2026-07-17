package torrent

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/url"
	"time"

	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"

	"github.com/anacrolix/torrent"
)

const (
	initialProbeBytes = 1 << 20
	maxProbeBytes     = 16 << 20
	probeTailBytes    = 1 << 20
	urlProbeTimeout   = 20 * time.Second
)

type torrentSource struct {
	file      *torrent.File
	registry  *rangeServer
	token     string
	url       string
	readahead int64
	onRead    func(string, int64)
}

type filePieceWindow struct {
	index int
	state torrent.PieceState
}

func newTorrentSource(file *torrent.File, registry *rangeServer, readaheadLimit int64, onRead func(string, int64), onError func(error)) (*torrentSource, error) {
	readahead := targetReadaheadBytes(file, readaheadLimit)
	token, sourceURL, err := registry.register(file, readahead, onRead, onError)
	if err != nil {
		return nil, err
	}
	return &torrentSource{
		file:      file,
		registry:  registry,
		token:     token,
		url:       sourceURL,
		readahead: readahead,
		onRead:    onRead,
	}, nil
}

func (s *torrentSource) URL() string {
	return s.url
}

func (s *torrentSource) URLForJob(jobID string) string {
	if jobID == "" {
		return s.url
	}
	return s.url + "?job=" + url.QueryEscape(jobID)
}

func (s *torrentSource) Close() {
	if s == nil || s.registry == nil {
		return
	}
	s.registry.unregister(s.token)
	s.registry = nil
}

func (s *torrentSource) Probe(ctx context.Context) (info domain.MediaInfo, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			info = domain.MediaInfo{}
			err = fmt.Errorf("torrent probe panic: %v", recovered)
		}
	}()
	probeBytes := min(int64(initialProbeBytes), s.readahead)
	s.PrefetchRange(0, probeBytes)
	if err := s.WaitRange(ctx, 0, probeBytes); err != nil {
		return domain.MediaInfo{}, err
	}

	reader := s.file.NewReader()
	sampleInfo, sampleErr := func() (domain.MediaInfo, error) {
		defer func() {
			if closeErr := closeTorrentReader(reader); closeErr != nil {
				log.Printf("torrent probe recovered reader close failure: %v", closeErr)
			}
		}()
		reader.SetContext(ctx)
		reader.SetReadahead(min(int64(maxProbeBytes), s.readahead))
		return (ffmpeg.SampleAnalyzer{}).Analyze(ctx, sourceProgressReader{Reader: reader, onRead: func(n int64) {
			if s.onRead != nil {
				s.onRead("", n)
			}
		}})
	}()
	if ctx.Err() != nil {
		return domain.MediaInfo{}, ctx.Err()
	}

	tailBytes := min(int64(probeTailBytes), s.readahead)
	if tailOffset := s.file.Length() - tailBytes; tailOffset > 0 {
		s.PrefetchRange(tailOffset, tailBytes)
	}
	if sampleErr == nil && sampleInfo.Duration > 0 {
		return s.refineDurationFromTail(ctx, sampleInfo), nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, urlProbeTimeout)
	defer cancel()
	info, urlErr := (ffmpeg.SampleAnalyzer{}).AnalyzeURL(probeCtx, s.url)
	if urlErr == nil {
		return s.refineDurationFromTail(ctx, info), nil
	}
	// A valid head sample is enough for progressive HLS even when the container
	// cannot expose a duration without an expensive scan to EOF.
	if sampleErr == nil && sampleInfo.VideoCodec != "" {
		tailCtx, cancelTail := context.WithTimeout(ctx, urlProbeTimeout)
		tailDuration, tailErr := (ffmpeg.SampleAnalyzer{}).AnalyzeTailDurationURL(tailCtx, s.url, sampleInfo.VideoTrackIndex)
		cancelTail()
		if tailErr == nil {
			sampleInfo.Duration = tailDuration
			sampleInfo.Seekable = true
			return sampleInfo, nil
		}
		sampleInfo.Duration = 0
		sampleInfo.Seekable = false
		return sampleInfo, nil
	}
	return domain.MediaInfo{}, urlErr
}

func (s *torrentSource) refineDurationFromTail(ctx context.Context, info domain.MediaInfo) domain.MediaInfo {
	tailCtx, cancel := context.WithTimeout(ctx, urlProbeTimeout)
	defer cancel()
	tailDuration, err := (ffmpeg.SampleAnalyzer{}).AnalyzeTailDurationURL(tailCtx, s.url, info.VideoTrackIndex)
	if err == nil && math.Abs(tailDuration-info.Duration) > 0.25 {
		info.Duration = tailDuration
		info.Seekable = true
	}
	return info
}

type sourceProgressReader struct {
	io.Reader
	onRead func(int64)
}

func (r sourceProgressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 && r.onRead != nil {
		r.onRead(int64(n))
	}
	return n, err
}

func closeTorrentReader(reader torrent.Reader) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("torrent reader panic: %v", recovered)
		}
	}()
	return reader.Close()
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

	pieceLength := s.file.Torrent().Info().PieceLength
	absoluteStart := s.file.Offset() + offset
	absoluteEnd := absoluteStart + length
	begin := int(absoluteStart / pieceLength)
	end := int((absoluteEnd-1)/pieceLength) + 1
	windows := make([]filePieceWindow, 0, end-begin)
	for index := begin; index < end; index++ {
		windows = append(windows, filePieceWindow{
			index: index,
			state: s.file.Torrent().Piece(index).State(),
		})
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

func targetReadaheadBytes(f *torrent.File, configured int64) int64 {
	ahead := configured
	if ahead <= 0 {
		ahead = 128 << 20
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
