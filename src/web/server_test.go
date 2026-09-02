package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cinemator/config"
	"cinemator/media"
	"cinemator/torrent"
)

func TestVersionEndpoint(t *testing.T) {
	server := Server{version: "0.3.1"}

	t.Run("returns build version", func(t *testing.T) {
		rec := httptest.NewRecorder()
		server.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		var response struct {
			Version string `json:"version"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if response.Version != "0.3.1" {
			t.Fatalf("version = %q, want 0.3.1", response.Version)
		}
	})

	t.Run("rejects other methods", func(t *testing.T) {
		rec := httptest.NewRecorder()
		server.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/version", nil))

		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Fatalf("Allow = %q, want %q", got, http.MethodGet)
		}
	})
}

type fakeTorrentManager struct {
	extendErr error
	deleteErr error
	filesErr  error
	events    <-chan struct{}
	prepare   string
}

func (m fakeTorrentManager) GetTorrentFiles(context.Context, string) ([]torrent.FileInfo, error) {
	return nil, m.filesErr
}

func (m fakeTorrentManager) GetMediaInfo(context.Context, string, int) (media.MediaInfo, error) {
	return media.MediaInfo{}, nil
}

func (m fakeTorrentManager) StartHLSPreparation(context.Context, string, int) error {
	return nil
}

func (m fakeTorrentManager) PrepareHlsStream(context.Context, string, int, int, int) (string, error) {
	return m.prepare, nil
}

func (m fakeTorrentManager) TouchStream(context.Context, string) {}

func (m fakeTorrentManager) ListDownloads(context.Context) ([]torrent.Download, error) {
	return []torrent.Download{}, nil
}

func (m fakeTorrentManager) ExtendDownload(context.Context, string, time.Duration) (torrent.Download, error) {
	if m.extendErr != nil {
		return torrent.Download{}, m.extendErr
	}
	return torrent.Download{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, nil
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

func TestHandleGetTorrentFilesReturnsUnsupportedMagnetError(t *testing.T) {
	server := Server{
		mgr: fakeTorrentManager{filesErr: torrent.ErrUnsupportedMagnetVersion},
		cfg: config.Load(),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/torrent/files?magnet=v2", nil)
	rec := httptest.NewRecorder()

	server.handleGetTorrentFiles(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != torrent.ErrUnsupportedMagnetVersion.Error() {
		t.Fatalf("body = %q, want %q", got, torrent.ErrUnsupportedMagnetVersion)
	}
}

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
			manager:    fakeTorrentManager{deleteErr: torrent.ErrBadDownloadID},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing extend target",
			method:     http.MethodPost,
			path:       "/api/downloads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/extend",
			manager:    fakeTorrentManager{extendErr: torrent.ErrDownloadNotFound},
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
			server := Server{mgr: tt.manager, cfg: config.Load()}
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
	}{
		{
			name:       "valid",
			method:     http.MethodGet,
			target:     "/api/hls/prepare?magnet=magnet&file=0&audio=1&subtitle=-1",
			wantStatus: http.StatusFound,
		},
		{
			name:       "malformed audio",
			method:     http.MethodGet,
			target:     "/api/hls/prepare?magnet=magnet&file=0&audio=default",
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
			name:       "start background preparation",
			method:     http.MethodPost,
			target:     "/api/hls/prepare?magnet=magnet&file=0",
			wantStatus: http.StatusAccepted,
		},
		{
			name:       "unsupported method",
			method:     http.MethodPut,
			target:     "/api/hls/prepare?magnet=magnet&file=0",
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  http.MethodGet + ", " + http.MethodPost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := Server{
				mgr: fakeTorrentManager{prepare: filepath.Join(t.TempDir(), "stream", "master.m3u8")},
				cfg: config.Load(),
			}
			req := httptest.NewRequest(tt.method, tt.target, nil)
			rec := httptest.NewRecorder()

			server.handlePrepareHlsStream(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Allow"); got != tt.wantAllow {
				t.Fatalf("Allow = %q, want %q", got, tt.wantAllow)
			}
		})
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

func TestAPIErrorStatusDefaultsToInternalServerError(t *testing.T) {
	if got := apiErrorStatus(errors.New("boom")); got != http.StatusInternalServerError {
		t.Fatalf("apiErrorStatus() = %d, want %d", got, http.StatusInternalServerError)
	}
}

func TestHandleDownloadEventsStreamsChangedEvents(t *testing.T) {
	events := make(chan struct{}, 1)
	server := Server{mgr: fakeTorrentManager{events: events}, cfg: config.Load()}
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
	data, err := readMediaPlaylistWithWait(ctx, playlist, time.Second)
	if err != nil {
		t.Fatalf("readMediaPlaylistWithWait() error = %v", err)
	}
	if !strings.Contains(string(data), "subs_00000.vtt") {
		t.Fatalf("playlist = %q, want segment uri", data)
	}
}

func TestHandleGetHlsChunkServesSelectionMasterWithoutMediaSegments(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CINEMATOR_HLS_PATH", root)
	dir := filepath.Join(root, "stream")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000\nindex.m3u8\n"
	if err := os.WriteFile(filepath.Join(dir, "master_a1_s0.m3u8"), []byte(master), 0644); err != nil {
		t.Fatal(err)
	}

	server := Server{mgr: fakeTorrentManager{}, cfg: config.Load()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/stream/master_a1_s0.m3u8", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	server.handleGetHlsChunk(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != master {
		t.Fatalf("master = %q, want %q", rec.Body.String(), master)
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
