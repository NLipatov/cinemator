package torrent

import (
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
	file      *torrent.File
	readahead int64
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

func (rs *rangeServer) register(file *torrent.File, readahead int64) (token, sourceURL string, err error) {
	for {
		token, err = randomToken()
		if err != nil {
			return "", "", err
		}

		rs.mu.Lock()
		if _, exists := rs.sources[token]; !exists {
			rs.sources[token] = rangeSource{file: file, readahead: readahead}
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

	if err := serveTorrentRange(w, r, src); err != nil && !isClientDisconnect(err) {
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

	reader := src.file.NewReader()
	reader.SetContext(r.Context())
	reader.SetReadahead(src.readahead)
	defer reader.Close()
	if _, err := reader.Seek(start, io.SeekStart); err != nil {
		return err
	}
	_, err = io.CopyN(w, reader, length)
	return err
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
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		strings.Contains(strings.ToLower(err.Error()), "broken pipe") ||
		strings.Contains(strings.ToLower(err.Error()), "connection reset by peer")
}
