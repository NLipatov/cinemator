package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cinemator/application"
	"cinemator/domain"
	"cinemator/presentation/settings"
)

type fakeTorrentManager struct {
	extendErr error
	deleteErr error
	events    <-chan struct{}
	prepare   string
	ensureErr error
	status    domain.HlsStatus
	statusErr error
	mediaInfo domain.MediaInfo
	ensured   chan [2]string
	ensure    func(streamDir, assetName string) error
	open      func(streamDir, assetName, version string) (application.HlsAsset, error)
}

func (m fakeTorrentManager) GetTorrentFiles(context.Context, string) ([]domain.FileInfo, error) {
	return nil, nil
}

func (m fakeTorrentManager) GetMediaInfo(context.Context, string, int) (domain.MediaInfo, error) {
	return m.mediaInfo, nil
}

func (m fakeTorrentManager) PrepareHlsStream(_ context.Context, _ string, _, _, _ int, start float64, _ bool) (string, error) {
	return m.prepare, nil
}

func (m fakeTorrentManager) OpenHlsAsset(_ context.Context, streamDir, assetName, version string) (application.HlsAsset, error) {
	if m.open != nil {
		return m.open(streamDir, assetName, version)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if m.ensured != nil {
			m.ensured <- [2]string{streamDir, assetName}
		}
		if m.ensure != nil {
			if err := m.ensure(streamDir, assetName); err != nil {
				return application.HlsAsset{}, err
			}
		} else if m.ensureErr != nil {
			return application.HlsAsset{}, m.ensureErr
		}
		path := filepath.Join(settings.NewSettings().HlsPath(), streamDir, assetName)
		file, err := os.Open(path)
		if err == nil {
			info, statErr := file.Stat()
			if statErr != nil {
				_ = file.Close()
				return application.HlsAsset{}, statErr
			}
			return application.HlsAsset{ReadSeekCloser: file, ModTime: info.ModTime()}, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return application.HlsAsset{}, err
		}
	}
	return application.HlsAsset{}, os.ErrNotExist
}

type blockingAsset struct {
	*bytes.Reader
	started chan struct{}
	release chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (a *blockingAsset) Read(data []byte) (int, error) {
	a.once.Do(func() { close(a.started) })
	<-a.release
	return a.Reader.Read(data)
}

func (a *blockingAsset) Close() error {
	close(a.closed)
	return nil
}

func (m fakeTorrentManager) GetHlsStatus(context.Context, string, float64) (domain.HlsStatus, error) {
	return m.status, m.statusErr
}

func (m fakeTorrentManager) TouchStream(context.Context, string) {}

func (m fakeTorrentManager) ListDownloads(context.Context) ([]domain.Download, error) {
	return []domain.Download{}, nil
}

func (m fakeTorrentManager) ExtendDownload(context.Context, string, time.Duration) (domain.Download, error) {
	if m.extendErr != nil {
		return domain.Download{}, m.extendErr
	}
	return domain.Download{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, nil
}

func (m fakeTorrentManager) DeleteDownload(context.Context, string) error {
	return m.deleteErr
}

func (m fakeTorrentManager) SubscribeDownloadEvents(ctx context.Context) <-chan struct{} {
	if m.events != nil {
		return m.events
	}
	ch := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}

func (m fakeTorrentManager) CleanupStreams() {}

func (m fakeTorrentManager) Close() error { return nil }

func TestHandleDownloadActionStatusMapping(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		manager    fakeTorrentManager
		wantStatus int
		wantAllow  string
	}{
		{
			name:       "bad delete id",
			method:     http.MethodDelete,
			path:       "/api/downloads/bad",
			manager:    fakeTorrentManager{deleteErr: domain.ErrBadDownloadID},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing extend target",
			method:     http.MethodPost,
			path:       "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend",
			manager:    fakeTorrentManager{extendErr: domain.ErrDownloadNotFound},
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "delete ok",
			method:     http.MethodDelete,
			path:       "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			manager:    fakeTorrentManager{},
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "unsupported download method",
			method:     http.MethodPut,
			path:       "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			manager:    fakeTorrentManager{},
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  http.MethodDelete,
		},
		{
			name:       "unsupported extend method",
			method:     http.MethodGet,
			path:       "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend",
			manager:    fakeTorrentManager{},
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  http.MethodPost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := HttpServer{mgr: tt.manager, settings: settings.NewSettings()}
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()

			server.handleDownloadAction(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantAllow != "" && rec.Header().Get("Allow") != tt.wantAllow {
				t.Fatalf("Allow = %q, want %q", rec.Header().Get("Allow"), tt.wantAllow)
			}
		})
	}
}

func TestHandlePrepareHlsStreamValidatesRequest(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		target     string
		wantStatus int
		wantAllow  string
		accept     string
	}{
		{
			name:       "valid",
			method:     http.MethodGet,
			target:     "/api/hls/prepare?magnet=magnet&file=0&audio=1&subtitle=-1",
			wantStatus: http.StatusOK,
			accept:     "application/json",
		},
		{
			name:       "valid legacy redirect",
			method:     http.MethodGet,
			target:     "/api/hls/prepare?magnet=magnet&file=0",
			wantStatus: http.StatusFound,
		},
		{
			name:       "malformed audio",
			method:     http.MethodGet,
			target:     "/api/hls/prepare?magnet=magnet&file=0&audio=default",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "malformed start",
			method:     http.MethodGet,
			target:     "/api/hls/prepare?magnet=magnet&file=0&start=-1",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "malformed transcode mode",
			method:     http.MethodGet,
			target:     "/api/hls/prepare?magnet=magnet&file=0&transcode=yes",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid subtitle",
			method:     http.MethodGet,
			target:     "/api/hls/prepare?magnet=magnet&file=0&subtitle=-2",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "negative file",
			method:     http.MethodGet,
			target:     "/api/hls/prepare?magnet=magnet&file=-1",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unsupported method",
			method:     http.MethodPost,
			target:     "/api/hls/prepare?magnet=magnet&file=0",
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  http.MethodGet,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := HttpServer{
				mgr:      fakeTorrentManager{prepare: filepath.Join(t.TempDir(), "stream", "master.m3u8")},
				settings: settings.NewSettings(),
			}
			req := httptest.NewRequest(tt.method, tt.target, nil)
			if tt.accept != "" {
				req.Header.Set("Accept", tt.accept)
			}
			rec := httptest.NewRecorder()

			server.handlePrepareHlsStream(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Allow"); got != tt.wantAllow {
				t.Fatalf("Allow = %q, want %q", got, tt.wantAllow)
			}
			if tt.accept == "application/json" && (!strings.Contains(rec.Body.String(), `"playlist":"/api/hls/stream/master.m3u8"`) || !strings.Contains(rec.Body.String(), `"stream":"stream"`) || !strings.Contains(rec.Body.String(), `"segmentDurationSeconds":6`) || !strings.Contains(rec.Body.String(), `"windowSegments":5`)) {
				t.Fatalf("body = %q", rec.Body.String())
			}
		})
	}
}

func TestHandleGetMediaInfoExposesPlaybackCapabilities(t *testing.T) {
	server := HttpServer{mgr: fakeTorrentManager{mediaInfo: domain.MediaInfo{
		VideoCodec:       "hevc",
		VideoCodecString: "hvc1.2.4.L153.B0",
		VideoProfile:     "Main 10",
		VideoLevel:       153,
		Width:            3840,
		Height:           2160,
		FrameRate:        23.976,
		PixelFormat:      "yuv420p10le",
		BitDepth:         10,
		HDR:              true,
		HDRFormat:        "HDR10",
		Bitrate:          20_000_000,
		AudioTracks:      []domain.AudioTrack{{Index: 0, Codec: "eac3", Channels: 6, SampleRate: 48000}},
	}}}
	req := httptest.NewRequest(http.MethodGet, "/api/media/info?magnet=magnet&file=0", nil)
	rec := httptest.NewRecorder()

	server.handleGetMediaInfo(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		`"videoCodec":"hevc"`,
		`"videoCodecString":"hvc1.2.4.L153.B0"`,
		`"width":3840`,
		`"frameRate":23.976`,
		`"hdrFormat":"HDR10"`,
		`"channels":6`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("media info missing %q: %s", want, rec.Body.String())
		}
	}
}

func TestHandleGetHlsStatus(t *testing.T) {
	want := domain.HlsStatus{Phase: "preparing", TargetSeconds: 72, BytesRead: 4096, ActivePeers: 2, TotalPeers: 4}
	server := HttpServer{mgr: fakeTorrentManager{status: want}, settings: settings.NewSettings()}
	req := httptest.NewRequest(http.MethodGet, "/api/hls/status/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_0_a0_s-1", nil)
	rec := httptest.NewRecorder()

	server.handleGetHlsStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"phase":"preparing"`) || !strings.Contains(rec.Body.String(), `"bytesRead":4096`) {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestHandleGetHlsStatusRejectsNestedPath(t *testing.T) {
	server := HttpServer{mgr: fakeTorrentManager{}, settings: settings.NewSettings()}
	req := httptest.NewRequest(http.MethodGet, "/api/hls/status/stream/asset", nil)
	rec := httptest.NewRecorder()

	server.handleGetHlsStatus(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestParseExtendDays(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		body    string
		want    int
		wantErr bool
	}{
		{
			name:   "query wins over body",
			target: "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend?days=3",
			body:   `{"days":30}`,
			want:   3,
		},
		{
			name:   "body days",
			target: "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend",
			body:   `{"days":30}`,
			want:   30,
		},
		{
			name:   "empty body defaults",
			target: "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend",
			body:   "",
			want:   7,
		},
		{
			name:   "zero body defaults",
			target: "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend",
			body:   `{"days":0}`,
			want:   7,
		},
		{
			name:    "zero query rejected",
			target:  "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend?days=0",
			body:    `{"days":7}`,
			wantErr: true,
		},
		{
			name:    "too large query rejected",
			target:  "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend?days=366",
			body:    "",
			wantErr: true,
		},
		{
			name:    "malformed query rejected",
			target:  "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend?days=soon",
			body:    "",
			wantErr: true,
		},
		{
			name:    "negative body rejected",
			target:  "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend",
			body:    `{"days":-1}`,
			wantErr: true,
		},
		{
			name:    "too large body rejected",
			target:  "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend",
			body:    `{"days":366}`,
			wantErr: true,
		},
		{
			name:    "malformed body rejected",
			target:  "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend",
			body:    `{`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.target, strings.NewReader(tt.body))

			got, err := parseExtendDays(req)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseExtendDays() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseExtendDays() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseExtendDays() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDownloadErrorStatusDefaultsToInternalServerError(t *testing.T) {
	if got := downloadErrorStatus(errors.New("boom")); got != http.StatusInternalServerError {
		t.Fatalf("downloadErrorStatus() = %d, want %d", got, http.StatusInternalServerError)
	}
}

func TestHandleDownloadEventsStreamsChangedEvents(t *testing.T) {
	events := make(chan struct{}, 1)
	server := HttpServer{mgr: fakeTorrentManager{events: events}, settings: settings.NewSettings()}
	testServer := httptest.NewServer(http.HandlerFunc(server.handleDownloadEvents))
	defer testServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testServer.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	resp, err := testServer.Client().Do(req)
	if err != nil {
		t.Fatalf("GET download events error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	reader := bufio.NewReader(resp.Body)
	if block := readSSEBlock(t, reader); !strings.Contains(block, "event: changed\n") {
		t.Fatalf("initial SSE block = %q, want changed event", block)
	}

	events <- struct{}{}
	if block := readSSEBlock(t, reader); !strings.Contains(block, "event: changed\n") {
		t.Fatalf("changed SSE block = %q, want changed event", block)
	}
}

func TestReadMediaPlaylistWithWaitWaitsForSegment(t *testing.T) {
	dir := t.TempDir()
	playlist := filepath.Join(dir, "subs.m3u8")
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n"), 0644); err != nil {
		t.Fatalf("write empty playlist: %v", err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		data := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.000,\nsubs_00000.vtt\n"
		_ = os.WriteFile(playlist, []byte(data), 0644)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data, err := readHlsAssetWithWait(ctx, func() (application.HlsAsset, error) {
		file, err := os.Open(playlist)
		if err != nil {
			return application.HlsAsset{}, err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return application.HlsAsset{}, err
		}
		return application.HlsAsset{ReadSeekCloser: file, ModTime: info.ModTime()}, nil
	}, time.Second, true)
	if err != nil {
		t.Fatalf("readHlsAssetWithWait() error = %v", err)
	}
	if !strings.Contains(string(data), "subs_00000.vtt") {
		t.Fatalf("playlist = %q, want segment uri", data)
	}
}

func TestReadHlsAssetWithWaitReturnsTerminalErrorImmediately(t *testing.T) {
	want := errors.New("generation failed")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := readHlsAssetWithWait(ctx, func() (application.HlsAsset, error) {
		return application.HlsAsset{}, want
	}, time.Hour, true)
	if !errors.Is(err, want) {
		t.Fatalf("readHlsAssetWithWait() error = %v, want %v", err, want)
	}
}

func TestReadHlsAssetWithWaitReportsPreparationTimeout(t *testing.T) {
	_, err := readHlsAssetWithWait(context.Background(), func() (application.HlsAsset, error) {
		return application.HlsAsset{}, os.ErrNotExist
	}, 0, true)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("readHlsAssetWithWait() error = %v, want deadline exceeded", err)
	}
}

func TestHandleGetHlsPlaylistReportsTerminalBackendError(t *testing.T) {
	dir := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_0_a0_s-1"
	server := HttpServer{mgr: fakeTorrentManager{ensureErr: errors.New("generation failed")}}
	req := httptest.NewRequest(http.MethodGet, "/"+dir+"/index.m3u8", nil)
	rec := httptest.NewRecorder()

	server.handleGetHlsChunk(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func TestHandleGetHlsChunkEnsuresOnDemandAsset(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	dir := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_0_a0_s-1"
	asset := "direct_000003_0000.m4s"
	if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, dir, asset), []byte("segment"), 0644); err != nil {
		t.Fatal(err)
	}
	ensured := make(chan [2]string, 1)
	server := HttpServer{
		mgr:      fakeTorrentManager{ensured: ensured},
		settings: settings.NewSettings(),
	}
	req := httptest.NewRequest(http.MethodGet, "/"+dir+"/"+asset, nil)
	rec := httptest.NewRecorder()

	server.handleGetHlsChunk(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if got := <-ensured; got != [2]string{dir, asset} {
		t.Fatalf("OpenHlsAsset() args = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("Content-Type = %q, want video/mp4", got)
	}
}

func TestHandleGetHlsChunkKeepsLeaseThroughResponse(t *testing.T) {
	dir := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_0_a0_s-1"
	asset := &blockingAsset{
		Reader:  bytes.NewReader([]byte("segment")),
		started: make(chan struct{}),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	opened := make(chan [3]string, 1)
	server := HttpServer{mgr: fakeTorrentManager{open: func(streamDir, assetName, version string) (application.HlsAsset, error) {
		opened <- [3]string{streamDir, assetName, version}
		return application.HlsAsset{ReadSeekCloser: asset, ModTime: time.Now()}, nil
	}}}
	req := httptest.NewRequest(http.MethodGet, "/"+dir+"/chunk_000003.ts?v=generation", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.handleGetHlsChunk(rec, req)
		close(done)
	}()

	<-asset.started
	select {
	case <-asset.closed:
		t.Fatal("asset lease closed before response read completed")
	default:
	}
	close(asset.release)
	<-done
	if got := <-opened; got != [3]string{dir, "chunk_000003.ts", "generation"} {
		t.Fatalf("OpenHlsAsset() args = %q", got)
	}
	select {
	case <-asset.closed:
	default:
		t.Fatal("asset lease was not closed after response")
	}
}

func TestHandleGetHlsChunkRetriesWhenCacheEvictsAssetBeforeOpen(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	dir := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_0_a0_s-1"
	asset := "chunk_000003.ts"
	assetPath := filepath.Join(root, dir, asset)
	if err := os.MkdirAll(filepath.Dir(assetPath), 0755); err != nil {
		t.Fatal(err)
	}
	ensureCalls := 0
	server := HttpServer{
		mgr: fakeTorrentManager{ensure: func(_, _ string) error {
			ensureCalls++
			if ensureCalls == 2 {
				return os.WriteFile(assetPath, []byte("regenerated"), 0644)
			}
			return nil
		}},
		settings: settings.NewSettings(),
	}
	req := httptest.NewRequest(http.MethodGet, "/"+dir+"/"+asset, nil)
	rec := httptest.NewRecorder()

	server.handleGetHlsChunk(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "regenerated" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if ensureCalls != 2 {
		t.Fatalf("OpenHlsAsset() generation calls = %d, want 2", ensureCalls)
	}
}

func TestHandleGetHlsChunkSignalsPlaylistReload(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	dir := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_0_a0_s-1"
	server := HttpServer{
		mgr: fakeTorrentManager{ensure: func(_, _ string) error {
			return domain.ErrHlsPlaylistChanged
		}},
		settings: settings.NewSettings(),
	}
	req := httptest.NewRequest(http.MethodGet, "/"+dir+"/seek_000003.ts", nil)
	rec := httptest.NewRecorder()

	server.handleGetHlsChunk(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestHandleGetHlsChunkDoesNotCacheMediaPlaylist(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	dir := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_0_a0_s-1"
	playlist := filepath.Join(root, dir, "index.m3u8")
	if err := os.MkdirAll(filepath.Dir(playlist), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:6.0,\nchunk_000000.ts\n"), 0644); err != nil {
		t.Fatal(err)
	}
	server := HttpServer{mgr: fakeTorrentManager{}, settings: settings.NewSettings()}
	req := httptest.NewRequest(http.MethodGet, "/"+dir+"/index.m3u8", nil)
	rec := httptest.NewRecorder()

	server.handleGetHlsChunk(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func readSSEBlock(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var block strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString() error = %v", err)
		}
		if line == "\n" || line == "\r\n" {
			return block.String()
		}
		block.WriteString(line)
	}
}
