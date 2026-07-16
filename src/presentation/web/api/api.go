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
	"math"
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
	auth     *authenticator
}

func NewHttpServer(settings settings.Settings) (*HttpServer, error) {
	auth, err := newAuthenticator(settings.PasswordHash(), settings.SessionSecret())
	if err != nil {
		return nil, fmt.Errorf("configure authentication: %w", err)
	}
	mgr, err := torrent.NewManager(settings)
	if err != nil {
		return nil, err
	}
	return &HttpServer{
		mgr:      mgr,
		settings: settings,
		auth:     auth,
	}, nil
}

func (s *HttpServer) Run() error {
	port := s.settings.HttpPort()
	if port < 0 || port > 65535 {
		return errors.New("invalid port")
	}

	listenPort := fmt.Sprintf(":%d", port)
	log.Printf("Server listening on %s", listenPort)
	server := &http.Server{
		Addr:              listenPort,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	return server.ListenAndServe()
}

func (s *HttpServer) handler() http.Handler {
	clientDir := "presentation/web/client/index"
	staticDir := "presentation/web/client/static"

	app := http.NewServeMux()
	app.Handle("/", http.FileServer(http.Dir(clientDir)))
	app.Handle("/favicon.ico", http.FileServer(http.Dir(staticDir)))
	app.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir(filepath.Join(staticDir, "assets")))))
	app.HandleFunc("/api/auth/logout", s.handleAuthLogout)
	app.HandleFunc("/api/torrent/files", s.handleGetTorrentFiles)
	app.HandleFunc("/api/media/info", s.handleGetMediaInfo)
	app.HandleFunc("/api/downloads", s.handleListDownloads)
	app.HandleFunc("/api/downloads/events", s.handleDownloadEvents)
	app.HandleFunc("/api/downloads/", s.handleDownloadAction)
	app.HandleFunc("/api/hls/prepare", s.handlePrepareHlsStream)
	app.HandleFunc("/api/hls/status/", s.handleGetHlsStatus)
	app.Handle("/api/hls/", http.StripPrefix("/api/hls/", http.HandlerFunc(s.handleGetHlsChunk)))

	root := http.NewServeMux()
	root.Handle("/favicon.ico", http.FileServer(http.Dir(staticDir)))
	root.Handle("/index.css", http.FileServer(http.Dir(clientDir)))
	root.Handle("/login.js", http.FileServer(http.Dir(clientDir)))
	root.HandleFunc("/login", s.handleLoginPage)
	root.HandleFunc("/api/auth/status", s.handleAuthStatus)
	root.HandleFunc("/api/auth/login", s.handleAuthLogin)
	root.Handle("/", s.requireAuthentication(app))
	return root
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
	startSeconds, err := parseStartSeconds(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	playlist, err := s.mgr.PrepareHlsStream(r.Context(), magnet, fileIndex, audioTrack, subtitleTrack, startSeconds)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	streamDir := filepath.Base(filepath.Dir(playlist))
	playlistURL := "/api/hls/" + streamDir + "/" + filepath.Base(playlist)
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, struct {
			Playlist string `json:"playlist"`
			Stream   string `json:"stream"`
		}{Playlist: playlistURL, Stream: streamDir})
		return
	}
	http.Redirect(w, r, playlistURL, http.StatusFound)
}

func (s *HttpServer) handleGetHlsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	streamDir := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/hls/status/"), "/")
	if streamDir == "" || strings.Contains(streamDir, "/") {
		http.Error(w, "bad stream", http.StatusBadRequest)
		return
	}
	targetSeconds := -1.0
	if raw := r.URL.Query().Get("target"); raw != "" {
		value, parseErr := strconv.ParseFloat(raw, 64)
		if parseErr != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			http.Error(w, "bad target", http.StatusBadRequest)
			return
		}
		targetSeconds = value
	}
	status, err := s.mgr.GetHlsStatus(r.Context(), streamDir, targetSeconds)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, status)
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
	const waitTimeout = 10 * time.Minute
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

	parts := strings.SplitN(clean, "/", 2)
	streamDir := parts[0]
	assetName := ""
	if len(parts) == 2 {
		assetName = parts[1]
	}

	// Track activity for cleanup/keepalive
	if dir := streamDir; dir != "" {
		s.mgr.TouchStream(r.Context(), dir)
	}

	if strings.HasSuffix(clean, ".m3u8") {
		w.Header().Set("Cache-Control", "no-store")
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
	var file *os.File
	var openErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.mgr.EnsureHlsAsset(r.Context(), streamDir, assetName); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("prepare HLS asset %s: %v", clean, err)
			}
			w.Header().Set("Retry-After", "1")
			http.Error(w, "chunk not available", http.StatusServiceUnavailable)
			return
		}
		file, openErr = os.Open(fullPath)
		if openErr == nil {
			break
		}
		if !errors.Is(openErr, os.ErrNotExist) {
			http.Error(w, "chunk not found", http.StatusNotFound)
			return
		}
	}
	if file == nil {
		http.Error(w, "chunk not found", http.StatusNotFound)
		return
	}
	defer file.Close()
	info, statErr := file.Stat()
	if statErr != nil {
		http.Error(w, "chunk not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	http.ServeContent(w, r, assetName, info.ModTime(), file)
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

func parseStartSeconds(r *http.Request) (float64, error) {
	raw := r.URL.Query().Get("start")
	if raw == "" {
		return 0, nil
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, errors.New("bad start time")
	}
	return seconds, nil
}
