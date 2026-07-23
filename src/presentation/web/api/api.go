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
	"net"
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

func (s *HttpServer) Run(ctx context.Context) (result error) {
	defer func() { result = errors.Join(result, s.mgr.Close()) }()
	port := s.settings.HttpPort()
	if port < 0 || port > 65535 {
		return errors.New("invalid port")
	}

	listenPort := fmt.Sprintf(":%d", port)
	log.Printf("Server listening on %s", listenPort)
	server := &http.Server{
		Addr:              listenPort,
		Handler:           s.handler(),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.ListenAndServe() }()
	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		result = errors.Join(err, server.Close())
	}
	if err := <-serveResult; !errors.Is(err, http.ErrServerClosed) {
		result = errors.Join(result, err)
	}
	return result
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
	forceTranscode := r.URL.Query().Get("transcode") == "1"
	if value := r.URL.Query().Get("transcode"); value != "" && value != "0" && value != "1" {
		http.Error(w, "bad transcode mode", http.StatusBadRequest)
		return
	}

	playlist, err := s.mgr.PrepareHlsStream(r.Context(), magnet, fileIndex, audioTrack, subtitleTrack, startSeconds, forceTranscode)
	if err != nil {
		log.Printf("prepare HLS stream: %v", err)
		status := hlsErrorStatus(err)
		http.Error(w, hlsErrorMessage(status), status)
		return
	}
	streamDir := filepath.Base(filepath.Dir(playlist))
	playlistURL := "/api/hls/" + streamDir + "/" + filepath.Base(playlist)
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, struct {
			Playlist               string  `json:"playlist"`
			Stream                 string  `json:"stream"`
			SegmentDurationSeconds float64 `json:"segmentDurationSeconds"`
			WindowSegments         int     `json:"windowSegments"`
		}{
			Playlist:               playlistURL,
			Stream:                 streamDir,
			SegmentDurationSeconds: s.settings.HlsSegmentDuration().Seconds(),
			WindowSegments:         s.settings.HlsWindowSegments(),
		})
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
		code := hlsErrorStatus(err)
		if code >= http.StatusInternalServerError && code != http.StatusServiceUnavailable {
			log.Printf("get HLS status: %v", err)
		}
		http.Error(w, hlsErrorMessage(code), code)
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

func hlsErrorStatus(err error) int {
	switch {
	case errors.Is(err, domain.ErrBadHlsRequest):
		return http.StatusBadRequest
	case errors.Is(err, domain.ErrHlsStreamNotFound), errors.Is(err, os.ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrHlsAssetUnsupported):
		return http.StatusUnsupportedMediaType
	case errors.Is(err, domain.ErrHlsPlaylistChanged):
		return http.StatusConflict
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	case errors.Is(err, domain.ErrHlsTemporarilyUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func hlsErrorMessage(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad HLS request"
	case http.StatusNotFound:
		return "HLS stream not found"
	case http.StatusUnsupportedMediaType:
		return "HLS media is unsupported"
	case http.StatusConflict:
		return "HLS playlist changed"
	case http.StatusGatewayTimeout:
		return "HLS preparation timed out"
	case http.StatusServiceUnavailable:
		return "HLS is temporarily unavailable"
	default:
		return "internal streaming error"
	}
}

func (s *HttpServer) handleGetHlsChunk(w http.ResponseWriter, r *http.Request) {
	const waitTimeout = 10 * time.Minute
	clean := path.Clean("/" + r.URL.Path)
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	parts := strings.SplitN(clean, "/", 2)
	if len(parts) != 2 || filepath.Base(parts[1]) != parts[1] {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	streamDir := parts[0]
	assetName := parts[1]
	version := r.URL.Query().Get("v")

	if strings.HasSuffix(clean, ".m3u8") {
		w.Header().Set("Cache-Control", "no-store")
		data, err := readHlsAssetWithWait(
			r.Context(),
			func() (application.HlsAsset, error) {
				return s.mgr.OpenHlsAsset(r.Context(), streamDir, assetName, version)
			},
			waitTimeout,
			path.Base(clean) != "master.m3u8",
		)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("prepare HLS playlist %s: %v", clean, err)
			}
			status := hlsErrorStatus(err)
			if status == http.StatusConflict {
				http.Error(w, "playlist changed", status)
			} else if status == http.StatusNotFound {
				http.Error(w, "playlist not found", status)
			} else if status == http.StatusGatewayTimeout {
				http.Error(w, "playlist preparation timed out", status)
			} else {
				http.Error(w, "playlist unavailable", status)
			}
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, writeErr := w.Write(data)
		if writeErr != nil {
			log.Printf("hls handling error: %v", writeErr)
		}
		return
	}
	asset, err := s.mgr.OpenHlsAsset(r.Context(), streamDir, assetName, version)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("prepare HLS asset %s: %v", clean, err)
		}
		status := hlsErrorStatus(err)
		if status == http.StatusConflict {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "playlist changed", status)
			return
		}
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "1")
		}
		http.Error(w, "chunk not available", status)
		return
	}
	defer asset.Close()
	switch filepath.Ext(assetName) {
	case ".m4s", ".mp4":
		w.Header().Set("Content-Type", "video/mp4")
	case ".ts":
		w.Header().Set("Content-Type", "video/mp2t")
	case ".vtt":
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	response := newIdleDeadlineWriter(w, 2*time.Minute)
	defer response.clearDeadline()
	http.ServeContent(response, r, assetName, asset.ModTime, asset)
}

type idleDeadlineWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
}

func newIdleDeadlineWriter(response http.ResponseWriter, timeout time.Duration) *idleDeadlineWriter {
	writer := &idleDeadlineWriter{
		ResponseWriter: response,
		controller:     http.NewResponseController(response),
		timeout:        timeout,
	}
	writer.refreshDeadline()
	return writer
}

func (w *idleDeadlineWriter) Write(data []byte) (int, error) {
	w.refreshDeadline()
	return w.ResponseWriter.Write(data)
}

func (w *idleDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *idleDeadlineWriter) refreshDeadline() {
	_ = w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
}

func (w *idleDeadlineWriter) clearDeadline() {
	_ = w.controller.SetWriteDeadline(time.Time{})
}

func readHlsAssetWithWait(ctx context.Context, open func() (application.HlsAsset, error), timeout time.Duration, requireSegment bool) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	delay := 120 * time.Millisecond
	for {
		asset, err := open()
		if errors.Is(err, domain.ErrHlsPlaylistChanged) {
			return nil, err
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			data, readErr := io.ReadAll(asset)
			closeErr := asset.Close()
			if readErr == nil && closeErr == nil && (!requireSegment || hls.HasSegment(string(data))) {
				return data, nil
			}
			if readErr != nil {
				return nil, readErr
			} else if closeErr != nil {
				return nil, closeErr
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		wait := min(delay, remaining)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 2*time.Second)
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
