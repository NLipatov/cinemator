package api

import (
	"cinemator/application"
	"cinemator/domain"
	"cinemator/infrastructure/hls"
	"cinemator/infrastructure/torrent"
	"cinemator/presentation/settings"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type HttpServer struct {
	mgr      application.TorrentManager
	settings settings.Settings
}

func NewHttpServer(settings settings.Settings) (*HttpServer, error) {
	mgr, err := torrent.NewManager(settings)
	if err != nil {
		return nil, err
	}
	return &HttpServer{
		mgr:      mgr,
		settings: settings,
	}, nil
}

func (s *HttpServer) Run() error {
	port := s.settings.HttpPort()
	if port < 0 || port > 65535 {
		return errors.New("invalid port")
	}

	// http-web client endpoints
	http.Handle("/", http.FileServer(http.Dir("presentation/web/client/index")))
	http.Handle("/favicon.ico", http.FileServer(http.Dir("presentation/web/client/static")))
	http.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir("presentation/web/client/static/assets"))))

	// http-api endpoints
	http.HandleFunc("/api/torrent/files", s.handleGetTorrentFiles)
	http.HandleFunc("/api/media/info", s.handleGetMediaInfo)
	http.HandleFunc("/api/downloads", s.handleListDownloads)
	http.HandleFunc("/api/downloads/events", s.handleDownloadEvents)
	http.HandleFunc("/api/downloads/", s.handleDownloadAction)
	http.HandleFunc("/api/hls/prepare", s.handlePrepareHlsStream)
	http.Handle("/api/hls/", http.StripPrefix("/api/hls/", http.HandlerFunc(s.handleGetHlsChunk)))

	listenPort := fmt.Sprintf(":%d", port)
	log.Printf("Server listening on %s", listenPort)
	return http.ListenAndServe(listenPort, nil)
}

func (s *HttpServer) handleGetTorrentFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	magnet := r.URL.Query().Get("magnet")
	if magnet == "" {
		http.Error(w, "magnet required", 400)
		return
	}
	files, err := s.mgr.GetTorrentFiles(r.Context(), magnet)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(files)
}

func (s *HttpServer) handleGetMediaInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	magnet, fileIndex, err := parseMagnetAndFile(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	info, err := s.mgr.GetMediaInfo(r.Context(), magnet, fileIndex)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (s *HttpServer) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	downloads, err := s.mgr.ListDownloads(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, downloads)
}

func (s *HttpServer) handleDownloadEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	events := s.mgr.SubscribeDownloadEvents(r.Context())
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")

	if err := writeSSEEvent(w, "changed"); err != nil {
		return
	}
	flusher.Flush()

	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			if err := writeSSEEvent(w, "changed"); err != nil {
				return
			}
			flusher.Flush()
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *HttpServer) handleDownloadAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/downloads/")
	if strings.Trim(rest, "/") == "" {
		s.handleListDownloads(w, r)
		return
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "download id required", http.StatusBadRequest)
		return
	}
	id := parts[0]

	if len(parts) == 1 {
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", http.MethodDelete)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.mgr.DeleteDownload(r.Context(), id); err != nil {
			http.Error(w, err.Error(), downloadErrorStatus(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if len(parts) == 2 && parts[1] == "extend" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		days, err := parseExtendDays(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		download, err := s.mgr.ExtendDownload(r.Context(), id, time.Duration(days)*24*time.Hour)
		if err != nil {
			http.Error(w, err.Error(), downloadErrorStatus(err))
			return
		}
		writeJSON(w, download)
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

func (s *HttpServer) handlePrepareHlsStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	magnet, fileIndex, err := parseMagnetAndFile(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	audioTrack, err := parseTrackIndex(r, "audio", 0, -1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	subtitleTrack, err := parseTrackIndex(r, "subtitle", -1, -1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	playlist, err := s.mgr.PrepareHlsStream(r.Context(), magnet, fileIndex, audioTrack, subtitleTrack)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(
		w, r,
		"/api/hls/"+filepath.Base(filepath.Dir(playlist))+"/"+filepath.Base(playlist),
		http.StatusFound)
}

func parseExtendDays(r *http.Request) (int, error) {
	const (
		defaultDays = 7
		maxDays     = 365
	)
	if raw := r.URL.Query().Get("days"); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days <= 0 || days > maxDays {
			return 0, errors.New("days must be between 1 and 365")
		}
		return days, nil
	}

	var body struct {
		Days int `json:"days"`
	}
	if r.Body == nil {
		return defaultDays, nil
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return defaultDays, nil
		}
		return 0, errors.New("bad extend request")
	}
	if body.Days == 0 {
		return defaultDays, nil
	}
	if body.Days < 0 || body.Days > maxDays {
		return 0, errors.New("days must be between 1 and 365")
	}
	return body.Days, nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writeSSEEvent(w io.Writer, event string) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: {}\n\n", event)
	return err
}

func downloadErrorStatus(err error) int {
	if errors.Is(err, domain.ErrBadDownloadID) {
		return http.StatusBadRequest
	}
	if errors.Is(err, domain.ErrDownloadNotFound) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func (s *HttpServer) handleGetHlsChunk(w http.ResponseWriter, r *http.Request) {
	const waitTimeout = 30 * time.Second
	clean := path.Clean("/" + r.URL.Path)
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	fullPath := filepath.Join(s.settings.HlsPath(), clean)
	hlsRoot := filepath.Clean(s.settings.HlsPath()) + string(os.PathSeparator)
	if !strings.HasPrefix(fullPath, hlsRoot) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Track activity for cleanup/keepalive
	if dir := strings.SplitN(clean, "/", 2)[0]; dir != "" {
		s.mgr.TouchStream(r.Context(), dir)
	}

	if strings.HasSuffix(clean, ".m3u8") {
		var data []byte
		var err error
		if path.Base(clean) == "master.m3u8" {
			data, err = readWithWait(r.Context(), fullPath, waitTimeout)
		} else {
			data, err = readMediaPlaylistWithWait(r.Context(), fullPath, waitTimeout)
		}
		if err != nil {
			http.Error(w, "playlist not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, writeErr := w.Write(data)
		if writeErr != nil {
			log.Printf("hls handling error: %v", writeErr)
		}
		return
	}
	if err := waitForPath(r.Context(), fullPath, waitTimeout); err != nil {
		http.Error(w, "chunk not found", 404)
		return
	}
	http.ServeFile(w, r, fullPath)
}

func waitForPath(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(120 * time.Millisecond):
		}
	}
}

func readWithWait(ctx context.Context, path string, timeout time.Duration) ([]byte, error) {
	if err := waitForPath(ctx, path, timeout); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func readMediaPlaylistWithWait(ctx context.Context, path string, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil && hls.HasSegment(string(data)) {
			return data, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("playlist segment not ready")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(120 * time.Millisecond):
		}
	}
}

func parseMagnetAndFile(r *http.Request) (string, int, error) {
	magnet := r.URL.Query().Get("magnet")
	idx := r.URL.Query().Get("file")
	if magnet == "" || idx == "" {
		return "", 0, errors.New("magnet and file required")
	}
	fileIndex, err := strconv.Atoi(idx)
	if err != nil || fileIndex < 0 {
		return "", 0, errors.New("bad file index")
	}
	return magnet, fileIndex, nil
}

func parseTrackIndex(r *http.Request, name string, defaultValue, minimum int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return defaultValue, nil
	}
	index, err := strconv.Atoi(raw)
	if err != nil || index < minimum {
		return 0, fmt.Errorf("bad %s track index", name)
	}
	return index, nil
}
