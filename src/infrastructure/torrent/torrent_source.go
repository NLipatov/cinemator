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
	urlProbeTimeout   = 20 * time.Second
)

type torrentSource struct {
	file      *torrent.File
	registry  *rangeServer
	demand    *pieceDemand
	token     string
	url       string
	readahead int64
	onRequest func(string, int64, int64)
	onRead    func(string, int64, int64)
}

type filePieceWindow struct {
	index int
	state torrent.PieceState
}

func newTorrentSource(file *torrent.File, registry *rangeServer, demand *pieceDemand, readaheadLimit int64, readaheadFor func(string) int64, onRequest func(string, int64, int64), onRead func(string, int64, int64), onError func(error)) (*torrentSource, error) {
	readahead := targetReadaheadBytes(file, readaheadLimit)
	token, sourceURL, err := registry.register(file, readahead, readaheadFor, onRequest, onRead, onError)
	if err != nil {
		return nil, err
	}
	return &torrentSource{
		file:      file,
		registry:  registry,
		demand:    demand,
		token:     token,
		url:       sourceURL,
		readahead: readahead,
		onRequest: onRequest,
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
				s.onRead("", 0, n)
			}
		}})
	}()
	if ctx.Err() != nil {
		return domain.MediaInfo{}, ctx.Err()
	}

	if sampleErr == nil && sampleInfo.VideoCodec != "" {
		return sampleInfo, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, urlProbeTimeout)
	defer cancel()
	info, urlErr := (ffmpeg.SampleAnalyzer{}).AnalyzeURL(probeCtx, s.url)
	if urlErr == nil {
		return info, nil
	}
	return domain.MediaInfo{}, urlErr
}

func (s *torrentSource) refineDurationFromTail(ctx context.Context, info domain.MediaInfo) (domain.MediaInfo, error) {
	tailCtx, cancel := context.WithTimeout(ctx, urlProbeTimeout)
	defer cancel()
	tailDuration, err := (ffmpeg.SampleAnalyzer{}).AnalyzeTailDurationURL(tailCtx, s.url, info.VideoTrackIndex)
	if err != nil {
		return info, err
	}
	if math.Abs(tailDuration-info.Duration) > 0.25 {
		info.Duration = tailDuration
		info.Seekable = true
	}
	return info, nil
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

func (s *torrentSource) WaitRange(ctx context.Context, offset, length int64) error {
	offset, length = clampRange(offset, length, s.file.Length())
	if length <= 0 {
		return nil
	}

	windows := s.rangeWindows(offset, length)
	if len(windows) == 0 {
		return nil
	}
	release := func() {}
	if s.demand != nil {
		release = s.demand.acquire(s.file.Torrent(), windows[0].index, windows[len(windows)-1].index+1)
	}
	defer release()
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

func (s *torrentSource) rangePieceCounts(offset, length int64) (missing, total int) {
	for _, window := range s.rangeWindows(offset, length) {
		total++
		if !window.state.Ok || !window.state.Complete {
			missing++
		}
	}
	return missing, total
}

func (s *torrentSource) requestedPieceBytes(pieces map[int]bool) (cacheBytes, peerBytes int64) {
	if s == nil {
		return 0, 0
	}
	for index, initiallyComplete := range pieces {
		piece := s.file.Torrent().Piece(index)
		state := piece.State()
		if !state.Ok || !state.Complete {
			continue
		}
		length := piece.Info().Length()
		if initiallyComplete {
			cacheBytes += length
		} else {
			peerBytes += length
		}
	}
	return cacheBytes, peerBytes
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
	spec.Trackers = supportedTrackerTiers(spec.Trackers)
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

func supportedTrackerTiers(tiers [][]string) [][]string {
	filtered := make([][]string, 0, len(tiers))
	for _, tier := range tiers {
		trackers := make([]string, 0, len(tier))
		for _, tracker := range tier {
			u, err := url.Parse(tracker)
			if err != nil {
				continue
			}
			switch u.Scheme {
			case "http", "https", "udp", "udp4", "udp6", "ws", "wss":
				trackers = append(trackers, tracker)
			}
		}
		if len(trackers) != 0 {
			filtered = append(filtered, trackers)
		}
	}
	return filtered
}
