package api

import (
	"cinemator/application"
	"cinemator/domain"
	"cinemator/infrastructure/torrent"
	"cinemator/presentation/settings"
	"cinemator/presentation/web/dto"
	"cinemator/presentation/web/mapping/mappers"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	mgr             application.TorrentManager
	fileInfoMapper  application.Mapper[domain.FileInfo, dto.FileInfo]
	mediaInfoMapper *mappers.MediaInfoMapper
	settings        settings.Settings
}

func NewHttpServer(settings settings.Settings) (*HttpServer, error) {
	mgr, err := torrent.NewManager(settings)
	if err != nil {
		return nil, err
	}
	return &HttpServer{
		mgr:             mgr,
		fileInfoMapper:  mappers.NewFileInfoMapper(),
		mediaInfoMapper: mappers.NewMediaInfoMapper(),
		settings:        settings,
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

	// http-api endpoints
	http.HandleFunc("/api/torrent/files", s.handleGetTorrentFiles)
	http.HandleFunc("/api/media/info", s.handleGetMediaInfo)
	http.HandleFunc("/api/hls/prepare", s.handlePrepareHlsStream)
	http.Handle("/api/hls/", http.StripPrefix("/api/hls/", http.HandlerFunc(s.handleGetHlsChunk)))

	listenPort := fmt.Sprintf(":%d", port)
	log.Printf("Server listening on %s", listenPort)
	return http.ListenAndServe(listenPort, nil)
}

func (s *HttpServer) handleGetTorrentFiles(w http.ResponseWriter, r *http.Request) {
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
	_ = json.NewEncoder(w).Encode(s.fileInfoMapper.MapArray(files))
}

func (s *HttpServer) handleGetMediaInfo(w http.ResponseWriter, r *http.Request) {
	magnet := r.URL.Query().Get("magnet")
	idx := r.URL.Query().Get("file")
	if magnet == "" || idx == "" {
		http.Error(w, "magnet and file required", 400)
		return
	}
	fileIndex, err := strconv.Atoi(idx)
	if err != nil {
		http.Error(w, "bad file index", 400)
		return
	}
	info, err := s.mgr.GetMediaInfo(r.Context(), magnet, fileIndex)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.mediaInfoMapper.Map(info))
}

func (s *HttpServer) handlePrepareHlsStream(w http.ResponseWriter, r *http.Request) {
	magnet := r.URL.Query().Get("magnet")
	idx := r.URL.Query().Get("file")
	if magnet == "" || idx == "" {
		http.Error(w, "magnet and file required", 400)
		return
	}
	fileIndex, err := strconv.Atoi(idx)
	if err != nil {
		http.Error(w, "bad file index", 400)
		return
	}

	audioTrack := 0
	if a := r.URL.Query().Get("audio"); a != "" {
		if parsed, err := strconv.Atoi(a); err == nil {
			audioTrack = parsed
		}
	}

	subtitleTrack := -1
	if sub := r.URL.Query().Get("subtitle"); sub != "" {
		if parsed, err := strconv.Atoi(sub); err == nil {
			subtitleTrack = parsed
		}
	}

	playlist, _, _, err := s.mgr.PrepareHlsStream(r.Context(), magnet, fileIndex, audioTrack, subtitleTrack)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(
		w, r,
		"/api/hls/"+filepath.Base(filepath.Dir(playlist))+"/"+filepath.Base(playlist),
		http.StatusFound)
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
		data, err := readWithWait(r.Context(), fullPath, waitTimeout)
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
