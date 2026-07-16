package torrent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
)

type rangeServer struct {
	baseURL string
	srv     *http.Server
	mu      sync.RWMutex
	sources map[string]rangeSource
}

type rangeSource struct {
	file      rangeFile
	readahead int64
	onRead    func(int64)
	onError   func(error)
}

type rangeFile interface {
	Length() int64
	NewReader() torrent.Reader
}

func newRangeServer() (*rangeServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	rs := &rangeServer{
		baseURL: "http://" + ln.Addr().String(),
		sources: make(map[string]rangeSource),
	}
	rs.srv = &http.Server{
		Handler:           rs,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := rs.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("torrent range server stopped: %v", err)
		}
	}()
	return rs, nil
}

func (rs *rangeServer) register(file *torrent.File, readahead int64, onRead func(int64), onError func(error)) (token, sourceURL string, err error) {
	for {
		token, err = randomToken()
		if err != nil {
			return "", "", err
		}

		rs.mu.Lock()
		if _, exists := rs.sources[token]; !exists {
			rs.sources[token] = rangeSource{file: file, readahead: readahead, onRead: onRead, onError: onError}
			rs.mu.Unlock()
			name := url.PathEscape(filepath.Base(file.DisplayPath()))
			return token, rs.baseURL + "/source/" + token + "/" + name, nil
		}
		rs.mu.Unlock()
	}
}

func (rs *rangeServer) unregister(token string) {
	if token == "" {
		return
	}
	rs.mu.Lock()
	delete(rs.sources, token)
	rs.mu.Unlock()
}

func (rs *rangeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/source/")
	if rest == r.URL.Path || rest == "" {
		http.NotFound(w, r)
		return
	}
	token := strings.SplitN(rest, "/", 2)[0]

	rs.mu.RLock()
	src, ok := rs.sources[token]
	rs.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err := fmt.Errorf("torrent source panic: %v", recovered)
			if src.onError != nil {
				src.onError(err)
			}
			log.Printf("range server panic: %v\n%s", err, debug.Stack())
		}
	}()

	if err := serveTorrentRange(w, r, src); err != nil && !isClientDisconnect(err) {
		if src.onError != nil {
			src.onError(err)
		}
		log.Printf("range server: %v", err)
	}
}

func serveTorrentRange(w http.ResponseWriter, r *http.Request, src rangeSource) error {
	size := src.file.Length()
	start, end, partial, err := parseByteRange(r.Header.Get("Range"), size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return nil
	}

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	if size == 0 {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return nil
	}

	length := end - start + 1
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return nil
	}

	return copyTorrentRange(r.Context(), w, src, start, length)
}

const maxRangeReadAttempts = 16

type torrentReadPanicError struct {
	value any
}

func (e torrentReadPanicError) Error() string {
	return fmt.Sprintf("torrent reader panic: %v", e.value)
}

// A bounded file cache can evict a piece that the torrent client previously
// marked complete. The first read detects that stale completion and returns an
// unexpected EOF after correcting it. Reopening the reader lets the client
// request the missing piece without changing its storage strategy at runtime.
func copyTorrentRange(ctx context.Context, dst io.Writer, src rangeSource, start, length int64) error {
	var written int64
	for attempt := 1; attempt <= maxRangeReadAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := readTorrentRangeAttempt(ctx, dst, src, start+written, length-written)
		written += n
		if written == length {
			if _, recovered := readErr.(torrentReadPanicError); recovered {
				log.Printf("range server recovered reader panic after completing response: %v", readErr)
				return nil
			}
			return readErr
		}
		_, recoveredPanic := readErr.(torrentReadPanicError)
		if !recoveredPanic && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return readErr
		}
		if attempt == maxRangeReadAttempts {
			return fmt.Errorf("torrent range remained unavailable after %d attempts: %w", attempt, readErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}

func readTorrentRangeAttempt(ctx context.Context, dst io.Writer, src rangeSource, start, length int64) (n int64, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = torrentReadPanicError{value: recovered}
		}
	}()

	reader := src.file.NewReader()
	reader.SetContext(ctx)
	reader.SetReadahead(src.readahead)
	if _, err := reader.Seek(start, io.SeekStart); err != nil {
		_ = reader.Close()
		return 0, err
	}
	n, readErr := io.CopyN(progressWriter{Writer: dst, onWrite: src.onRead}, reader, length)
	closeErr := reader.Close()
	if readErr != nil {
		return n, readErr
	}
	return n, closeErr
}

type progressWriter struct {
	io.Writer
	onWrite func(int64)
}

func (w progressWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 && w.onWrite != nil {
		w.onWrite(int64(n))
	}
	return n, err
}

func parseByteRange(value string, size int64) (start, end int64, partial bool, err error) {
	if value == "" {
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") {
		return 0, 0, false, fmt.Errorf("unsupported range unit")
	}
	if size <= 0 {
		return 0, 0, false, fmt.Errorf("empty source")
	}

	spec := strings.TrimPrefix(value, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, 0, false, fmt.Errorf("multiple ranges are not supported")
	}
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return 0, 0, false, fmt.Errorf("bad range")
	}

	first, last := spec[:dash], spec[dash+1:]
	if first == "" {
		suffixLen, err := strconv.ParseInt(last, 10, 64)
		if err != nil || suffixLen <= 0 {
			return 0, 0, false, fmt.Errorf("bad suffix range")
		}
		if suffixLen >= size {
			return 0, size - 1, true, nil
		}
		return size - suffixLen, size - 1, true, nil
	}

	start, err = strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, fmt.Errorf("bad range start")
	}
	if last == "" {
		return start, size - 1, true, nil
	}
	end, err = strconv.ParseInt(last, 10, 64)
	if err != nil || end < start {
		return 0, 0, false, fmt.Errorf("bad range end")
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true, nil
}

func randomToken() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func isClientDisconnect(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		strings.Contains(strings.ToLower(err.Error()), "broken pipe") ||
		strings.Contains(strings.ToLower(err.Error()), "connection reset by peer")
}
