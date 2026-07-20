package torrent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestServeTorrentRangeReportsBodyProgress(t *testing.T) {
	var read int64
	src := rangeSource{
		file:      fakeRangeFile{data: []byte("0123456789")},
		readahead: 128,
		onRead: func(jobID string, offset, n int64) {
			if jobID != "job-7" {
				t.Fatalf("job id = %q", jobID)
			}
			if offset != 2 {
				t.Fatalf("offset = %d, want 2", offset)
			}
			read += n
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/source/token/file.mkv?job=job-7", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()

	if err := serveTorrentRange(rec, req, src); err != nil {
		t.Fatalf("serveTorrentRange() error = %v", err)
	}
	if read != 4 {
		t.Fatalf("reported bytes = %d, want 4", read)
	}
}

func TestServeTorrentRangeUsesJobSpecificReadahead(t *testing.T) {
	var requested int64
	src := rangeSource{
		file:      fakeRangeFile{data: []byte("0123456789")},
		readahead: 8,
		readaheadFor: func(jobID string) int64 {
			if jobID != "startup" {
				t.Fatalf("job id = %q, want startup", jobID)
			}
			return 3
		},
		onRequest: func(_ string, _, length int64) {
			requested = length
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/source/token/file.mkv?job=startup", nil)
	rec := httptest.NewRecorder()

	if err := serveTorrentRange(rec, req, src); err != nil {
		t.Fatalf("serveTorrentRange() error = %v", err)
	}
	if requested != 3 {
		t.Fatalf("requested readahead = %d, want 3", requested)
	}
}

func TestServeTorrentRangeAdvancesTheReportedDemandWithTheReader(t *testing.T) {
	data := bytes.Repeat([]byte{'x'}, 2*progressReportBytes)
	requests := make([]int64, 0, 3)
	src := rangeSource{
		file:      fakeRangeFile{data: data},
		readahead: progressReportBytes,
		onRequest: func(_ string, offset, _ int64) {
			requests = append(requests, offset)
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/source/token/file.mkv?job=streaming", nil)
	rec := httptest.NewRecorder()

	if err := serveTorrentRange(rec, req, src); err != nil {
		t.Fatalf("serveTorrentRange() error = %v", err)
	}
	if len(requests) < 2 || requests[0] != 0 || requests[1] <= requests[0] {
		t.Fatalf("reported demand offsets = %v, want a moving range", requests)
	}
}

func TestServeTorrentRangeRetriesAfterEvictedPieceRead(t *testing.T) {
	file := &flakyRangeFile{data: []byte("0123456789")}
	src := rangeSource{file: file, readahead: 128}
	req := httptest.NewRequest(http.MethodGet, "/source/token/file.mkv", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()

	if err := serveTorrentRange(rec, req, src); err != nil {
		t.Fatalf("serveTorrentRange() error = %v", err)
	}
	if body := rec.Body.String(); body != "2345" {
		t.Fatalf("body = %q, want %q", body, "2345")
	}
	if file.readers != 2 {
		t.Fatalf("readers = %d, want one retry", file.readers)
	}
	if file.closes != 2 {
		t.Fatalf("closed readers = %d, want 2", file.closes)
	}
}

func TestServeTorrentRangeRetriesAfterReaderPanic(t *testing.T) {
	file := &flakyRangeFile{data: []byte("0123456789"), panicFirstRead: true}
	src := rangeSource{file: file, readahead: 128}
	req := httptest.NewRequest(http.MethodGet, "/source/token/file.mkv", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()

	if err := serveTorrentRange(rec, req, src); err != nil {
		t.Fatalf("serveTorrentRange() error = %v", err)
	}
	if body := rec.Body.String(); body != "2345" {
		t.Fatalf("body = %q, want %q", body, "2345")
	}
	if file.readers != 2 {
		t.Fatalf("readers = %d, want one retry", file.readers)
	}
}

func TestServeTorrentRangeResumesAfterPartialReaderPanic(t *testing.T) {
	file := &flakyRangeFile{data: []byte("0123456789"), panicAfterPartial: true}
	src := rangeSource{file: file, readahead: 128}
	req := httptest.NewRequest(http.MethodGet, "/source/token/file.mkv", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()

	if err := serveTorrentRange(rec, req, src); err != nil {
		t.Fatalf("serveTorrentRange() error = %v", err)
	}
	if body := rec.Body.String(); body != "2345" {
		t.Fatalf("body = %q, want %q", body, "2345")
	}
	if file.readers != 2 {
		t.Fatalf("readers = %d, want one retry", file.readers)
	}
}

func TestRangeServerRecoversReaderClosePanicAfterCompleteResponse(t *testing.T) {
	reported := make(chan error, 1)
	rs := &rangeServer{sources: map[string]rangeSource{
		"token": {
			file: panicRangeFile{data: []byte("0123")},
			onError: func(err error) {
				reported <- err
			},
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/source/token/file.mkv", nil)
	rec := httptest.NewRecorder()

	rs.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "0123" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	select {
	case err := <-reported:
		t.Fatalf("completed response reported fatal error: %v", err)
	default:
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

type flakyRangeFile struct {
	data              []byte
	readers           int
	closes            int
	panicFirstRead    bool
	panicAfterPartial bool
}

type panicRangeFile struct {
	data []byte
}

func (f panicRangeFile) Length() int64 {
	return int64(len(f.data))
}

func (f panicRangeFile) NewReader() torrent.Reader {
	return &fakeTorrentReader{Reader: bytes.NewReader(f.data), panicOnClose: true}
}

func (f *flakyRangeFile) Length() int64 {
	return int64(len(f.data))
}

func (f *flakyRangeFile) NewReader() torrent.Reader {
	f.readers++
	reader := &fakeTorrentReader{Reader: bytes.NewReader(f.data), onClose: func() { f.closes++ }}
	if f.readers == 1 {
		if f.panicFirstRead {
			reader.panicOnRead = true
		} else if f.panicAfterPartial {
			reader.maxRead = 2
			reader.panicAfterReads = 1
		} else {
			reader.fail = io.ErrUnexpectedEOF
		}
	}
	return reader
}

type fakeTorrentReader struct {
	*bytes.Reader
	ctx             context.Context
	fail            error
	panicOnClose    bool
	panicOnRead     bool
	onClose         func()
	maxRead         int
	panicAfterReads int
	reads           int
}

func (r *fakeTorrentReader) Read(b []byte) (int, error) {
	if r.panicOnRead {
		panic("reader read failed")
	}
	if r.panicAfterReads > 0 && r.reads >= r.panicAfterReads {
		panic("reader read failed after partial response")
	}
	r.reads++
	if r.fail != nil {
		err := r.fail
		r.fail = nil
		return 0, err
	}
	if r.maxRead > 0 && len(b) > r.maxRead {
		b = b[:r.maxRead]
	}
	return r.Reader.Read(b)
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
	if r.onClose != nil {
		r.onClose()
	}
	if r.panicOnClose {
		panic("reader close failed")
	}
	return nil
}

func (r *fakeTorrentReader) SetReadahead(_ int64) {}

func (r *fakeTorrentReader) SetReadaheadFunc(_ torrent.ReadaheadFunc) {}

func (r *fakeTorrentReader) SetResponsive() {}
