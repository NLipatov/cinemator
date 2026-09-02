package web

import (
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

	"cinemator/config"
	"cinemator/media"
	"cinemator/torrent"
)

type Server struct {
	mgr     torrentManager
	cfg     config.Config
	auth    *authenticator
	version string
}

func NewServer(cfg config.Config, version string) (*Server, error) {
	auth, err := newAuthenticator(cfg.PasswordHash, cfg.SessionSecret)
	if err != nil {
		return nil, fmt.Errorf("configure authentication: %w", err)
	}
	mgr, err := torrent.NewManager(cfg)
	if err != nil {
		return nil, err
	}
	return &Server{
		mgr:     mgr,
		cfg:     cfg,
		auth:    auth,
		version: version,
	}, nil
}

func (s *Server) Run() error {
	port := s.cfg.HTTPPort
	if port < 0 || port > 65535 {
		return errors.New("invalid port")
	}

	listenPort := fmt.Sprintf(":%d", port)
	log.Printf("Server listening on %s", listenPort)
	return http.ListenAndServe(listenPort, s.handler())
}

func (s *Server) handler() http.Handler {
	clientDir := "web/client/index"
	staticDir := "web/client/static"

	app := http.NewServeMux()
	app.Handle("/", http.FileServer(http.Dir(clientDir)))
	app.Handle("/favicon.ico", http.FileServer(http.Dir(staticDir)))
	app.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir(filepath.Join(staticDir, "assets")))))
	app.HandleFunc("/api/version", s.handleGetVersion)
	app.HandleFunc("/api/auth/logout", s.handleAuthLogout)
	app.HandleFunc("/api/torrent/files", s.handleGetTorrentFiles)
	app.HandleFunc("/api/media/info", s.handleGetMediaInfo)
	app.HandleFunc("/api/downloads", s.handleListDownloads)
	app.HandleFunc("/api/downloads/events", s.handleDownloadEvents)
	app.HandleFunc("/api/downloads/", s.handleDownloadAction)
	app.HandleFunc("/api/hls/prepare", s.handlePrepareHlsStream)
	app.Handle("/api/hls/", http.StripPrefix("/api/hls/", http.HandlerFunc(s.handleGetHlsChunk)))

	root := http.NewServeMux()
	root.Handle("/favicon.ico", http.FileServer(http.Dir(staticDir)))
	root.Handle("/index.css", http.FileServer(http.Dir(clientDir)))
	root.Handle("/login.js", http.FileServer(http.Dir(clientDir)))
	root.Handle("/sign-in-approval.js", http.FileServer(http.Dir(clientDir)))
	root.HandleFunc("/login", s.handleLoginPage)
	root.HandleFunc("GET /sign-in-approvals/{approvalToken}", s.handleSignInApprovalPage)
	root.HandleFunc("/api/auth/status", s.handleAuthStatus)
	root.HandleFunc("/api/auth/login", s.handleAuthLogin)
	root.HandleFunc("POST /api/auth/sign-in-requests", s.handleStartSignInRequest)
	root.HandleFunc("GET /api/auth/sign-in-requests/{deviceToken}/qr", s.handleSignInRequestQRCode)
	root.HandleFunc("POST /api/auth/sign-in-requests/{deviceToken}/session", s.handleSignInRequestSession)
	root.HandleFunc("GET /api/auth/sign-in-approvals/{approvalToken}", s.handleSignInApproval)
	root.HandleFunc("POST /api/auth/sign-in-approvals/{approvalToken}", s.handleApproveSignInRequest)
	root.Handle("/", s.requireAuthentication(app))
	return root
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, struct {
		Version string `json:"version"`
	}{Version: s.version})
}

func (s *Server) handleGetTorrentFiles(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, err.Error(), apiErrorStatus(err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(files)
}

func (s *Server) handleGetMediaInfo(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, err.Error(), apiErrorStatus(err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (s *Server) handleListDownloads(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleDownloadEvents(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleDownloadAction(w http.ResponseWriter, r *http.Request) {
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
			http.Error(w, err.Error(), apiErrorStatus(err))
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
			http.Error(w, err.Error(), apiErrorStatus(err))
			return
		}
		writeJSON(w, download)
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

func (s *Server) handlePrepareHlsStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	magnet, fileIndex, err := parseMagnetAndFile(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodPost {
		if err := s.mgr.StartHLSPreparation(r.Context(), magnet, fileIndex); err != nil {
			http.Error(w, err.Error(), apiErrorStatus(err))
			return
		}
		w.WriteHeader(http.StatusAccepted)
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
	if err := s.mgr.StartHLSPreparation(r.Context(), magnet, fileIndex); err != nil {
		http.Error(w, err.Error(), apiErrorStatus(err))
		return
	}

	playlist, err := s.mgr.PrepareHlsStream(r.Context(), magnet, fileIndex, audioTrack, subtitleTrack)
	if err != nil {
		http.Error(w, err.Error(), apiErrorStatus(err))
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

func apiErrorStatus(err error) int {
	if errors.Is(err, torrent.ErrBadDownloadID) || errors.Is(err, torrent.ErrUnsupportedMagnetVersion) {
		return http.StatusBadRequest
	}
	if errors.Is(err, torrent.ErrDownloadNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, torrent.ErrDownloadNotReady) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func (s *Server) handleGetHlsChunk(w http.ResponseWriter, r *http.Request) {
	const waitTimeout = 30 * time.Second
	clean := path.Clean("/" + r.URL.Path)
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	fullPath := filepath.Join(s.cfg.HLSPath, clean)
	hlsRoot := filepath.Clean(s.cfg.HLSPath) + string(os.PathSeparator)
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
		playlistName := path.Base(clean)
		if playlistName == "master.m3u8" || strings.HasPrefix(playlistName, "master_") {
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
		if err == nil && media.HasSegment(string(data)) {
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
