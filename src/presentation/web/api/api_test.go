package api

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cinemator/domain"
	"cinemator/presentation/settings"
)

type fakeTorrentManager struct {
	extendErr error
	deleteErr error
	events    <-chan struct{}
}

func (m fakeTorrentManager) GetTorrentFiles(context.Context, string) ([]domain.FileInfo, error) {
	return nil, nil
}

func (m fakeTorrentManager) GetMediaInfo(context.Context, string, int) (domain.MediaInfo, error) {
	return domain.MediaInfo{}, nil
}

func (m fakeTorrentManager) PrepareHlsStream(context.Context, string, int, int, int) (string, string, context.CancelFunc, error) {
	return "", "", nil, nil
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
	data, err := readMediaPlaylistWithWait(ctx, playlist, time.Second)
	if err != nil {
		t.Fatalf("readMediaPlaylistWithWait() error = %v", err)
	}
	if !strings.Contains(string(data), "subs_00000.vtt") {
		t.Fatalf("playlist = %q, want segment uri", data)
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
