package torrent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
)

func TestParseByteRange(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		size    int64
		start   int64
		end     int64
		partial bool
	}{
		{name: "full", size: 100, start: 0, end: 99},
		{name: "open ended", header: "bytes=10-", size: 100, start: 10, end: 99, partial: true},
		{name: "closed", header: "bytes=10-19", size: 100, start: 10, end: 19, partial: true},
		{name: "clamped", header: "bytes=90-999", size: 100, start: 90, end: 99, partial: true},
		{name: "suffix", header: "bytes=-20", size: 100, start: 80, end: 99, partial: true},
		{name: "large suffix", header: "bytes=-200", size: 100, start: 0, end: 99, partial: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, partial, err := parseByteRange(tt.header, tt.size)
			if err != nil {
				t.Fatalf("parseByteRange() error = %v", err)
			}
			if start != tt.start || end != tt.end || partial != tt.partial {
				t.Fatalf("parseByteRange() = %d, %d, %t; want %d, %d, %t", start, end, partial, tt.start, tt.end, tt.partial)
			}
		})
	}
}

func TestParseByteRangeRejectsInvalidRanges(t *testing.T) {
	tests := []string{
		"items=0-1",
		"bytes=200-201",
		"bytes=10-1",
		"bytes=-0",
		"bytes=0-1,2-3",
		"bytes=x-y",
	}

	for _, header := range tests {
		t.Run(header, func(t *testing.T) {
			if _, _, _, err := parseByteRange(header, 100); err == nil {
				t.Fatalf("parseByteRange(%q) succeeded, want error", header)
			}
		})
	}
}

func TestServeTorrentRangeHTTPBehavior(t *testing.T) {
	src := rangeSource{
		file:      fakeRangeFile{data: []byte("0123456789")},
		readahead: 128,
	}
	tests := []struct {
		name        string
		method      string
		rangeHeader string
		wantStatus  int
		wantRange   string
		wantLength  string
		wantBody    string
		wantEmpty   bool
	}{
		{
			name:       "full get",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantLength: "10",
			wantBody:   "0123456789",
		},
		{
			name:        "range get",
			method:      http.MethodGet,
			rangeHeader: "bytes=2-5",
			wantStatus:  http.StatusPartialContent,
			wantRange:   "bytes 2-5/10",
			wantLength:  "4",
			wantBody:    "2345",
		},
		{
			name:        "range head",
			method:      http.MethodHead,
			rangeHeader: "bytes=1-3",
			wantStatus:  http.StatusPartialContent,
			wantRange:   "bytes 1-3/10",
			wantLength:  "3",
			wantEmpty:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/source/token/file.mkv", nil)
			if tt.rangeHeader != "" {
				req.Header.Set("Range", tt.rangeHeader)
			}
			rr := httptest.NewRecorder()

			if err := serveTorrentRange(rr, req, src); err != nil {
				t.Fatalf("serveTorrentRange() error = %v", err)
			}
			res := rr.Result()
			defer res.Body.Close()

			if res.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", res.StatusCode, tt.wantStatus)
			}
			if got := res.Header.Get("Accept-Ranges"); got != "bytes" {
				t.Fatalf("Accept-Ranges = %q, want bytes", got)
			}
			if got := res.Header.Get("Content-Range"); got != tt.wantRange {
				t.Fatalf("Content-Range = %q, want %q", got, tt.wantRange)
			}
			if got := res.Header.Get("Content-Length"); got != tt.wantLength {
				t.Fatalf("Content-Length = %q, want %q", got, tt.wantLength)
			}
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantEmpty {
				if len(body) != 0 {
					t.Fatalf("body = %q, want empty", string(body))
				}
				return
			}
			if string(body) != tt.wantBody {
				t.Fatalf("body = %q, want %q", string(body), tt.wantBody)
			}
		})
	}
}

func TestUnregisterCancelsAndWaitsForActiveRequest(t *testing.T) {
	reader := &blockingTorrentReader{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	sourceCtx, sourceCancel := context.WithCancel(context.Background())
	rs := &rangeServer{
		sources: map[string]rangeSource{
			"token": {
				file:     blockingRangeFile{reader: reader},
				ctx:      sourceCtx,
				cancel:   sourceCancel,
				requests: &sync.WaitGroup{},
			},
		},
	}

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	request := httptest.NewRequest(http.MethodGet, "/source/token/file.mkv", nil).WithContext(requestCtx)
	handlerDone := make(chan struct{})
	go func() {
		rs.ServeHTTP(httptest.NewRecorder(), request)
		close(handlerDone)
	}()
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("range request did not start reading")
	}

	unregisterDone := make(chan struct{})
	go func() {
		rs.unregister("token")
		close(unregisterDone)
	}()
	select {
	case <-reader.closed:
	case <-unregisterDone:
		cancelRequest()
		<-handlerDone
		t.Fatal("unregister() returned while the active reader was still open")
	case <-time.After(time.Second):
		cancelRequest()
		<-handlerDone
		t.Fatal("unregister() did not cancel the active reader")
	}
	select {
	case <-unregisterDone:
	case <-time.After(time.Second):
		t.Fatal("unregister() did not return after the active reader closed")
	}
}

type fakeRangeFile struct {
	data []byte
}

func (f fakeRangeFile) Length() int64 {
	return int64(len(f.data))
}

func (f fakeRangeFile) NewReader() torrent.Reader {
	return &fakeTorrentReader{Reader: bytes.NewReader(f.data)}
}

type fakeTorrentReader struct {
	*bytes.Reader
	ctx context.Context
}

func (r *fakeTorrentReader) SetContext(ctx context.Context) {
	r.ctx = ctx
}

func (r *fakeTorrentReader) ReadContext(ctx context.Context, b []byte) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
		return r.Read(b)
	}
}

func (r *fakeTorrentReader) Close() error {
	return nil
}

func (r *fakeTorrentReader) SetReadahead(_ int64) {}

func (r *fakeTorrentReader) SetReadaheadFunc(_ torrent.ReadaheadFunc) {}

func (r *fakeTorrentReader) SetResponsive() {}

type blockingRangeFile struct {
	reader *blockingTorrentReader
}

func (f blockingRangeFile) Length() int64 {
	return 1
}

func (f blockingRangeFile) NewReader() torrent.Reader {
	return f.reader
}

type blockingTorrentReader struct {
	ctx       context.Context
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func (r *blockingTorrentReader) Read(_ []byte) (int, error) {
	r.startOnce.Do(func() { close(r.started) })
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *blockingTorrentReader) Seek(_ int64, _ int) (int64, error) {
	return 0, nil
}

func (r *blockingTorrentReader) SetContext(ctx context.Context) {
	r.ctx = ctx
}

func (r *blockingTorrentReader) ReadContext(ctx context.Context, b []byte) (int, error) {
	return r.Read(b)
}

func (r *blockingTorrentReader) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

func (r *blockingTorrentReader) SetReadahead(_ int64) {}

func (r *blockingTorrentReader) SetReadaheadFunc(_ torrent.ReadaheadFunc) {}

func (r *blockingTorrentReader) SetResponsive() {}
